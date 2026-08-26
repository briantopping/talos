// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"context"
	"fmt"
	"slices"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	blockpb "github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/block"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	configconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block/blockhelpers"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

// LVMPhysicalVolumeSpecController evaluates v1alpha1 LVMVolumeGroupConfig
// selectors against discovered volumes (whole disks and partitions) and emits
// one LVMPhysicalVolumeSpec per match.
type LVMPhysicalVolumeSpecController struct{}

// Name implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeSpecController) Name() string {
	return "storage.LVMPhysicalVolumeSpecController"
}

// Inputs implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeSpecController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineConfigType,
			ID:        optional.Some(config.ActiveID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.DiscoveredVolumeType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.DiskType,
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.SystemDiskType,
			ID:        optional.Some(block.SystemDiskID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.VolumeStatusType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeSpecController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: storage.LVMPhysicalVolumeSpecType,
			Kind: controller.OutputExclusive,
		},
		{
			Type: storage.LVMValidationErrorType,
			Kind: controller.OutputShared,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *LVMPhysicalVolumeSpecController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		if err := ctrl.reconcile(ctx, r, logger); err != nil {
			return err
		}
	}
}

// reconcile runs a single reconciliation pass.
func (ctrl *LVMPhysicalVolumeSpecController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	machineCfg, err := safe.ReaderGetByID[*config.MachineConfig](ctx, r, config.ActiveID)
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("get machine config: %w", err)
	}

	var vgDocs []configconfig.LVMVolumeGroupConfig

	if machineCfg != nil {
		vgDocs = machineCfg.Config().LVMVolumeGroupConfigs()
	}

	volumes, err := buildMatchContexts(ctx, r)
	if err != nil {
		return err
	}

	var machineConfig configconfig.Config

	if machineCfg != nil {
		machineConfig = machineCfg.Config()
	}

	resolver, err := buildEncryptionResolver(ctx, r, machineConfig, volumes)
	if err != nil {
		return err
	}

	r.StartTrackingOutputs()

	if err := ctrl.emitSpecs(ctx, r, logger, vgDocs, volumes, resolver); err != nil {
		return err
	}

	if err := r.CleanupOutputs(
		ctx,
		resource.NewMetadata(storage.NamespaceName, storage.LVMPhysicalVolumeSpecType, "", resource.VersionUndefined),
		resource.NewMetadata(storage.NamespaceName, storage.LVMValidationErrorType, "", resource.VersionUndefined),
	); err != nil {
		return fmt.Errorf("cleanup outputs: %w", err)
	}

	return nil
}

// emitSpecs evaluates every VG selector against the discovered volumes and
// writes PV specs for matches, recording overlap conflicts as validation
// errors.
func (ctrl *LVMPhysicalVolumeSpecController) emitSpecs(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	vgDocs []configconfig.LVMVolumeGroupConfig,
	volumes []blockhelpers.MatchContext,
	resolver encryptionResolver,
) error {
	// Per-device claim map: detect VGs whose selectors overlap (LVM forbids a
	// PV in two VGs).
	claimedBy := map[string]string{}

	// Conflicts recorded per losing VG, surfaced as LVMValidationError.
	conflicts := map[string]string{}

	for _, doc := range vgDocs {
		if doc.PhysicalVolumeSelector().IsZero() {
			continue
		}

		if err := ctrl.matchVolumesToVG(ctx, r, logger, doc, volumes, resolver, claimedBy, conflicts); err != nil {
			return err
		}
	}

	for vgName, msg := range conflicts {
		if err := ctrl.writeValidationError(ctx, r, vgName, msg); err != nil {
			return err
		}
	}

	return nil
}

// matchVolumesToVG evaluates the selector of a single VG doc against all
// volumes, updating claimedBy for matches and conflicts for overlaps.
func (ctrl *LVMPhysicalVolumeSpecController) matchVolumesToVG(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	doc configconfig.LVMVolumeGroupConfig,
	volumes []blockhelpers.MatchContext,
	resolver encryptionResolver,
	claimedBy map[string]string,
	conflicts map[string]string,
) error {
	selector := doc.PhysicalVolumeSelector()

	for _, vol := range volumes {
		matches, err := selector.EvalBool(celenv.VolumeLocator(), vol.CELContext)
		if err != nil {
			return fmt.Errorf("evaluate selector for VG %q: %w", doc.Name(), err)
		}

		if !matches {
			continue
		}

		// A partitioned whole disk can't be a PV; its partitions are matched
		// separately. Skip it so the reconciler doesn't try pvcreate on a
		// device that lvm rejects ("device is partitioned").
		if vol.Partitioned {
			logger.Debug(
				"skipping partitioned disk as PV candidate; its partitions are preferred",
				zap.String("device", vol.DevPath),
				zap.String("vg", doc.Name()),
			)

			continue
		}

		// An encrypted volume is selected by the properties of its ciphertext
		// partition (the GPT label the operator wrote), but the PV belongs on
		// the OPENED device. pvcreate is run without --yes, so handing it the
		// ciphertext would abort on the crypto_LUKS signature rather than
		// produce a PV.
		devPath, ready := resolver.resolve(vol.DevPath)
		if !ready {
			logger.Debug(
				"skipping encrypted volume that is not unlocked yet",
				zap.String("device", vol.DevPath),
				zap.String("vg", doc.Name()),
			)

			continue
		}

		if prev, ok := claimedBy[devPath]; ok && prev != doc.Name() {
			conflicts[doc.Name()] = fmt.Sprintf("device %q already claimed by volume group %q", devPath, prev)

			logger.Warn(
				"disk claimed by multiple LVM volume groups; skipping",
				zap.String("device", devPath),
				zap.String("first_vg", prev),
				zap.String("conflicting_vg", doc.Name()),
			)

			continue
		}

		claimedBy[devPath] = doc.Name()

		if err := ctrl.writePVSpec(ctx, r, devPath, doc.Name()); err != nil {
			return err
		}
	}

	return nil
}

func (ctrl *LVMPhysicalVolumeSpecController) writePVSpec(ctx context.Context, r controller.Runtime, devPath, vgName string) error {
	id := pvID(devPath)

	if err := safe.WriterModify(
		ctx, r,
		storage.NewLVMPhysicalVolumeSpec(storage.NamespaceName, id),
		func(s *storage.LVMPhysicalVolumeSpec) error {
			pvSpec := s.TypedSpec()
			pvSpec.Device = devPath
			pvSpec.VGName = vgName

			return nil
		},
	); err != nil {
		return fmt.Errorf("modify LVMPhysicalVolumeSpec %q: %w", id, err)
	}

	return nil
}

func (ctrl *LVMPhysicalVolumeSpecController) writeValidationError(ctx context.Context, r controller.Runtime, vgName, message string) error {
	if err := safe.WriterModify(
		ctx, r,
		storage.NewLVMValidationError(storage.NamespaceName, vgName),
		func(e *storage.LVMValidationError) error {
			spec := e.TypedSpec()
			spec.VGName = vgName
			spec.Message = message

			return nil
		},
	); err != nil {
		return fmt.Errorf("modify LVMValidationError %q: %w", vgName, err)
	}

	return nil
}

// buildMatchContexts lists discovered volumes, disks and the system disk, and
// delegates CEL context construction to blockhelpers.BuildMatchContexts. Every
// volume gets a `volume` variable; `disk` is bound to the real disk only for
// whole-disk volumes so disk-level predicates (e.g. disk.transport == "nvme")
// evaluate false against partitions rather than spanning the disk and all its
// partitions. Partitions are therefore selectable only via `volume.*`
// predicates (e.g. volume.partition_label), matching the documented contract.
func buildMatchContexts(ctx context.Context, r controller.Runtime) ([]blockhelpers.MatchContext, error) {
	disks, err := safe.ReaderListAll[*block.Disk](ctx, r)
	if err != nil {
		return nil, fmt.Errorf("list disks: %w", err)
	}

	volumes, err := safe.ReaderListAll[*block.DiscoveredVolume](ctx, r)
	if err != nil {
		return nil, fmt.Errorf("list discovered volumes: %w", err)
	}

	systemDiskDevPath := ""

	systemDisk, err := safe.ReaderGetByID[*block.SystemDisk](ctx, r, block.SystemDiskID)
	if err != nil && !state.IsNotFoundError(err) {
		return nil, fmt.Errorf("get system disk: %w", err)
	}

	if systemDisk != nil {
		systemDiskDevPath = systemDisk.TypedSpec().DevPath
	}

	return blockhelpers.BuildMatchContexts(slices.Collect(disks.All()), slices.Collect(volumes.All()), systemDiskDevPath)
}

// encryptionResolver maps the device a VG selector matched to the device a
// physical volume must actually be created on.
//
// A selector addresses an encrypted volume through the properties of its
// ciphertext partition -- `volume.partition_label`, the label the operator
// wrote -- because those are the stable, user-authored identifiers. The opened
// device carries none of them: it is DEVTYPE=disk, so DevicesController leaves
// its Parent empty and it has no partition label, and its /dev/dm-N path is
// assigned in open order. VolumeStatus is what bridges the two.
type encryptionResolver struct {
	// resolved maps VolumeStatus.Location to VolumeStatus.MountLocation for
	// volumes whose usable device differs from the one the selector sees.
	resolved map[string]string
	// pending holds Locations of encrypted volumes that are not open yet.
	pending map[string]struct{}
}

// resolve returns the device to create the PV on. The second return value is
// false when the volume is encrypted but not yet unlocked, in which case the
// caller must skip it: the ciphertext device is not a substitute.
func (e encryptionResolver) resolve(devPath string) (string, bool) {
	if _, notReady := e.pending[devPath]; notReady {
		return "", false
	}

	if mapped, ok := e.resolved[devPath]; ok {
		return mapped, true
	}

	return devPath, true
}

// buildEncryptionResolver indexes VolumeStatus by Location.
//
// HandleEncryption sets MountLocation == Location for unencrypted volumes and
// to the opened dm device for LUKS2 ones, so a device only gets an entry when
// its usable path differs from the one the selector matched, or when no usable
// path exists yet; every other device resolves to itself.
func buildEncryptionResolver(ctx context.Context, r controller.Runtime, cfg configconfig.Config, contexts []blockhelpers.MatchContext) (encryptionResolver, error) {
	statuses, err := safe.ReaderListAll[*block.VolumeStatus](ctx, r)
	if err != nil {
		return encryptionResolver{}, fmt.Errorf("list volume statuses: %w", err)
	}

	resolver := encryptionResolver{
		resolved: map[string]string{},
		pending:  map[string]struct{}{},
	}

	for status := range statuses.All() {
		spec := status.TypedSpec()

		if spec.Location == "" {
			continue
		}

		switch {
		case spec.MountLocation == "":
			// No usable device published yet. This is deliberately keyed on
			// MountLocation rather than EncryptionProvider: HandleEncryption sets
			// the provider only AFTER a successful open, so a volume that is
			// encrypted but still locked reports EncryptionProviderNone and would
			// otherwise look like a plain device whose ciphertext could be handed
			// to pvcreate.
			resolver.pending[spec.Location] = struct{}{}
		case spec.MountLocation != spec.Location:
			resolver.resolved[spec.Location] = spec.MountLocation
		}
	}

	// A VolumeStatus is not an authoritative statement that a device is plaintext:
	// it is created empty (Waiting, no Location), the controllers publishing it are
	// not ordered against this one, and it is destroyed on teardown while the
	// ciphertext DiscoveredVolume still exists. In all three windows the loop above
	// records nothing, and a missing entry would otherwise read as "plain device".
	//
	// The machine config does not have those windows, so configured-encrypted raw
	// volumes are classified from it directly: a device carrying such a volume's
	// partition label is unusable until its opened device is known.
	if err := markConfiguredEncrypted(contexts, cfg, &resolver); err != nil {
		return encryptionResolver{}, err
	}

	return resolver, nil
}

// encryptedVolumeMatchers collects, from the machine config, how to recognize the
// devices that configured-encrypted volumes will claim: partition labels for volumes
// provisioned as partitions, and disk selectors for volumes claiming a whole device.
func encryptedVolumeMatchers(cfg configconfig.Config) (map[string]struct{}, []cel.Expression) {
	var (
		labels    = map[string]struct{}{}
		selectors []cel.Expression
	)

	for _, doc := range cfg.RawVolumeConfigs() {
		if doc.Encryption() != nil {
			labels[constants.RawVolumePrefix+doc.Name()] = struct{}{}
		}
	}

	for _, doc := range cfg.SwapVolumeConfigs() {
		if doc.Encryption() != nil {
			labels[constants.SwapVolumePrefix+doc.Name()] = struct{}{}
		}
	}

	for _, doc := range cfg.UserVolumeConfigs() {
		if doc.Encryption() == nil {
			continue
		}

		if doc.Type().ValueOr(block.VolumeTypePartition) != block.VolumeTypeDisk {
			labels[constants.UserVolumePrefix+doc.Name()] = struct{}{}

			continue
		}

		if selector, ok := doc.Provisioning().DiskSelector().Get(); ok {
			selectors = append(selectors, selector)
		}
	}

	return labels, selectors
}

// markConfiguredEncrypted marks every device that a configured-encrypted volume will
// claim as pending, unless the volume manager has already published its opened device.
//
// Two shapes have to be covered, because Talos expresses encryption on both:
//
//   - a volume provisioned as a PARTITION (RawVolumeConfig, and UserVolumeConfig /
//     SwapVolumeConfig in their default form) is found by its partition label, which is
//     the volume ID the volume config controller stamps on it;
//   - a volume provisioned as a whole DISK (UserVolumeConfig with volumeType: disk) has
//     no partition and no label, and is found by evaluating the volume's own disk
//     selector -- the same expression the volume manager will use.
//
// The whole-disk shape is what an encrypted MD array looks like: the array is a
// block.Disk, the volume claims all of it, and nothing about it carries a label.
func markConfiguredEncrypted(contexts []blockhelpers.MatchContext, cfg configconfig.Config, resolver *encryptionResolver) error {
	if cfg == nil {
		return nil
	}

	labels, selectors := encryptedVolumeMatchers(cfg)

	if len(labels) == 0 && len(selectors) == 0 {
		return nil
	}

	for _, c := range contexts {
		if c.DevPath == "" {
			continue
		}

		if _, resolved := resolver.resolved[c.DevPath]; resolved {
			continue
		}

		matched, err := matchesEncryptedVolume(c, labels, selectors)
		if err != nil {
			return err
		}

		if matched {
			resolver.pending[c.DevPath] = struct{}{}
		}
	}

	return nil
}

// matchesEncryptedVolume reports whether a discovered device is one a configured
// encrypted volume will claim, by partition label or by the volume's disk selector.
func matchesEncryptedVolume(c blockhelpers.MatchContext, labels map[string]struct{}, selectors []cel.Expression) (bool, error) {
	if len(labels) > 0 {
		if spec, ok := c.CELContext["volume"].(*blockpb.DiscoveredVolumeSpec); ok && spec != nil {
			if _, found := labels[spec.PartitionLabel]; found {
				return true, nil
			}
		}
	}

	for i := range selectors {
		matches, err := selectors[i].EvalBool(celenv.VolumeLocator(), c.CELContext)
		if err != nil {
			return false, fmt.Errorf("evaluate encrypted volume disk selector: %w", err)
		}

		if matches {
			return true, nil
		}
	}

	return false, nil
}
