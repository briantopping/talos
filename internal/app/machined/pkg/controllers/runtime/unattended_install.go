// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/optional"
	"go.uber.org/zap"

	v1alpha1runtime "github.com/siderolabs/talos/internal/app/machined/pkg/runtime"
	"github.com/siderolabs/talos/internal/pkg/install"
	"github.com/siderolabs/talos/pkg/images"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/config/config"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block/blockhelpers"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	crires "github.com/siderolabs/talos/pkg/machinery/resources/cri"
	"github.com/siderolabs/talos/pkg/machinery/resources/runtime"
)

// UnattendedInstallController performs an unattended install driven by the UnattendedInstallConfig config document.
//
// It mirrors the legacy `.machine.install` install behavior, but is driven entirely by the multi-document
// config. It does NOT reboot the node after a successful install; reboot is handled separately.
type UnattendedInstallController struct {
	V1Alpha1Mode v1alpha1runtime.Mode

	// State is the resource state used to match the install disk.
	State state.State

	postInstallErr error

	// PostInstallFunc runs after the installer has written the disk. Its failure is reported
	// but does not mark the install failed: the disk is already written.
	PostInstallFunc func(ctx context.Context) error

	// ImageCacheWaitFunc waits for the image cache copy to finish. It is called ONCE,
	// after the retry loop, and deliberately NOT retried -- see the call site.
	ImageCacheWaitFunc func(ctx context.Context) error

	// InstalledFunc reports whether the node is already installed to disk.
	InstalledFunc func() bool

	// PlatformFunc returns the platform name (e.g. "metal", "aws", etc.).
	PlatformFunc func() string

	// InstallFunc performs the actual install of the given image to the given disk.
	//
	// It is a field to allow the install side-effect to be stubbed in tests.
	InstallFunc func(ctx context.Context, disk, image string, wipe bool) error

	// installMu provides single-flight semantics for the install: only one install may run at a time.
	installMu sync.Mutex
	// installDone records, in-memory, that the installer has already run this boot, so it is never run
	// twice even if the status resource read lags behind a just-written value.
	installDone bool
}

// NewUnattendedInstallController creates an UnattendedInstallController wired to the runtime, with the
// default install behavior (run the installer container, waiting for the image cache around it).
func NewUnattendedInstallController(rt v1alpha1runtime.Runtime) *UnattendedInstallController {
	resources := rt.State().V1Alpha2().Resources()

	return &UnattendedInstallController{
		V1Alpha1Mode:  rt.State().Platform().Mode(),
		State:         resources,
		InstalledFunc: rt.State().Machine().Installed,
		PlatformFunc:  rt.State().Platform().Name,
		InstallFunc: func(ctx context.Context, disk, image string, wipe bool) error {
			if err := crires.WaitForImageCache(ctx, resources); err != nil {
				return fmt.Errorf("failed to wait for the image cache: %w", err)
			}

			if err := install.RunInstallerContainer(
				disk,
				rt.State().Platform().Name(),
				image,
				rt.Config(),
				rt.ConfigContainer(),
				resources,
				crires.RegistryBuilder(resources),
				install.WithForce(true),
				install.WithZero(wipe),
				install.WithGrubUseUKICmdline(true),
			); err != nil {
				return err
			}

			return nil
		},
		// the install sequence (which reloads/flushes META after the installer) is skipped when the
		// UnattendedInstallConfig document drives the install, so merge the in-memory META with what
		// the installer wrote into the partition it just created. Separate from InstallFunc because
		// the disk is already written: a failure here must not re-run the installer.
		PostInstallFunc: func(ctx context.Context) error {
			meta := rt.State().Machine().Meta()

			if err := install.ReloadMeta(ctx, resources, meta); err != nil {
				return err
			}

			return install.SyncMeta(ctx, resources, meta)
		},
		ImageCacheWaitFunc: func(ctx context.Context) error {
			return crires.WaitForImageCacheCopy(ctx, resources)
		},
	}
}

// maxPostInstallAttempts bounds the merge retry so a permanently failing one does not
// consume the controller for the life of the boot.
//
// This bounds attempts, not time. Each attempt is ReloadMeta + SyncMeta, which carry
// their own 60s timeouts (internal/pkg/install/meta.go), so a permanently failing merge
// occupies about six minutes. UnattendedInstallStatus reads `installing` throughout,
// which correctly keeps the bootstrap gates closed, but on a machine with no console it
// is indistinguishable from a hang.
const maxPostInstallAttempts = 3

// postInstallRetryDelay spaces the attempts.
//
// Without it a fast failure -- a ReloadMeta error that is not the timeout -- would retry
// three times in microseconds, which is not a retry.
//
// A variable, not a const, so tests can drive the loop without paying for the sleep.
var postInstallRetryDelay = 2 * time.Second

// Name implements controller.Controller interface.
func (ctrl *UnattendedInstallController) Name() string {
	return "runtime.UnattendedInstallController"
}

// Inputs implements controller.Controller interface.
func (ctrl *UnattendedInstallController) Inputs() []controller.Input {
	return []controller.Input{
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineConfigType,
			ID:        optional.Some(config.ActiveID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: runtime.NamespaceName,
			Type:      runtime.ImageFactorySchematicType,
			ID:        optional.Some(runtime.ImageFactorySchematicID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: block.NamespaceName,
			Type:      block.DiskType,
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *UnattendedInstallController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: runtime.UnattendedInstallStatusType,
			Kind: controller.OutputExclusive,
		},
		{
			Type: runtime.RebootRequestType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
func (ctrl *UnattendedInstallController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	if ctrl.V1Alpha1Mode == v1alpha1runtime.ModeContainer {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		if err := ctrl.run(ctx, r, logger); err != nil {
			return err
		}
	}
}

func (ctrl *UnattendedInstallController) run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	cfg, err := safe.ReaderGetByID[*config.MachineConfig](ctx, r, config.ActiveID)
	if err != nil && !state.IsNotFoundError(err) {
		return fmt.Errorf("error getting machine config: %w", err)
	}

	var doc talosconfig.UnattendedInstallConfig

	if cfg != nil {
		doc = cfg.Config().UnattendedInstallConfig()
	}

	r.StartTrackingOutputs()

	if doc != nil {
		if err = ctrl.reconcile(ctx, r, logger, doc); err != nil {
			return err
		}
	}

	return safe.CleanupOutputs[*runtime.UnattendedInstallStatus](ctx, r)
}

//nolint:gocyclo
func (ctrl *UnattendedInstallController) reconcile(
	ctx context.Context,
	r controller.Runtime,
	logger *zap.Logger,
	doc talosconfig.UnattendedInstallConfig,
) error {
	// Once we have recorded a completed install for this boot, the install target is fixed.
	// A new disk later matching the selector must not flip the reported disk or trigger a reinstall.
	if existing, err := safe.ReaderGetByID[*runtime.UnattendedInstallStatus](ctx, r, runtime.UnattendedInstallStatusID); err != nil {
		if !state.IsNotFoundError(err) {
			return fmt.Errorf("error getting unattended install status: %w", err)
		}
	} else if phase := existing.TypedSpec().Phase; phase == runtime.UnattendedInstallPhaseInstalled || phase == runtime.UnattendedInstallPhaseWaitingForReboot || phase == runtime.UnattendedInstallPhaseInstalling {
		// the merge is retried, the installer is not: the measured failure is a timeout while
		// the array reassembles, and boot-time repopulation is gated on META being absent, so
		// a merge that never succeeds drops the in-memory values permanently

		// Installing is included: a controller restart mid-install must not start a second
		// installer against a disk whose volumes are open.
		// re-affirm the status (so CleanupOutputs retains it): the install target is fixed and a new
		// disk later matching the selector must not trigger a reinstall.
		//
		// the phase is preserved as is: `waiting-for-reboot` must never be downgraded to `installed`,
		// as the phase gates APIs which shouldn't run in the pre-reboot window (e.g. bootstrap).
		return ctrl.setStatus(ctx, r, doc, phase, nil)
	}

	if ctrl.InstalledFunc() {
		return ctrl.setStatus(ctx, r, doc, ctrl.installedPhase(doc), nil)
	}

	// resolve the target disk from the CEL selector against the discovered disks.
	// the selector still matches after the install, so the disk is re-resolved and reported in the
	// status after a reboot (the status resource is in-memory and gone after reboot).
	matchExpr := doc.VolumeSelector()

	matchedDisks, err := blockhelpers.MatchDisks(ctx, ctrl.State, &matchExpr)
	if err != nil {
		return fmt.Errorf("failed to match install disk: %w", err)
	}

	var disk string

	if len(matchedDisks) > 0 {
		if len(matchedDisks) > 1 {
			logger.Warn("multiple disks matched the install selector, using the first one",
				zap.Int("matched", len(matchedDisks)),
				zap.String("disk", matchedDisks[0].TypedSpec().DevPath),
			)
		}

		if disk, err = filepath.EvalSymlinks(matchedDisks[0].TypedSpec().DevPath); err != nil {
			return fmt.Errorf("failed to resolve disk symlink: %w", err)
		}
	}

	if len(matchedDisks) == 0 {
		// disks may not have been discovered yet; record and wait for the next event.
		return ctrl.setStatus(ctx, r, doc, runtime.UnattendedInstallPhasePending, fmt.Errorf("no disk matched the selector"))
	}

	// single-flight: only one install may run at a time.
	if !ctrl.installMu.TryLock() {
		// an install is already in progress; keep the status and wait for it to complete.
		return ctrl.setStatus(ctx, r, doc, runtime.UnattendedInstallPhaseInstalling, nil)
	}
	defer ctrl.installMu.Unlock()

	// the installer already ran this boot: don't run it again, just keep the status as installed.
	if ctrl.installDone {
		return ctrl.setStatus(ctx, r, doc, ctrl.installedPhase(doc), nil)
	}

	installerImage := doc.InstallerImage()
	if installerImage == "" {
		installerImage, err = ctrl.getInstallerFromBootEntry(ctx, r)
		if err != nil {
			return ctrl.setStatus(ctx, r, doc, runtime.UnattendedInstallPhaseFailed, fmt.Errorf("failed to determine installer image: %w", err))
		}

		logger.Warn("installer image not specified in config, using image from boot entry", zap.String("image", installerImage))
	}

	if err = ctrl.setStatus(ctx, r, doc, runtime.UnattendedInstallPhaseInstalling, nil); err != nil {
		return err
	}

	logger.Info("installing Talos", zap.String("disk", disk), zap.String("image", installerImage))

	if err = ctrl.InstallFunc(ctx, disk, installerImage, doc.VolumeWipe()); err != nil {
		if statErr := ctrl.setStatus(ctx, r, doc, runtime.UnattendedInstallPhaseFailed, err); statErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to flush install failure status: %w", statErr))
		}

		return fmt.Errorf("failed to run installer: %w", err)
	}

	ctrl.installDone = true

	// retried here, before the reboot is requested: afterwards the machine is going down and
	// a merge would race the teardown's unmount. ReloadMeta and SyncMeta are retried as a
	// pair -- SyncMeta alone would flush stale in-memory META over what the installer wrote.
	for attempt := range maxPostInstallAttempts {
		if ctrl.postInstallErr = ctrl.PostInstallFunc(ctx); ctrl.postInstallErr == nil {
			break
		}

		logger.Error("post-install step failed; the disk is written, so the installer is not re-run",
			zap.Int("attempt", attempt+1), zap.Error(ctrl.postInstallErr))

		if attempt+1 < maxPostInstallAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(postInstallRetryDelay):
			}
		}
	}

	// ONCE, and deliberately not retried.
	//
	// It is the last step, so by the time it can fail the META pair has already succeeded.
	// Retrying it re-runs two operations that worked, and a second ReloadMeta can fail while
	// the array is still settling after the repartition -- reporting a META timeout on a
	// machine whose META merged correctly. Its outcome is observable on ImageCacheConfig's
	// CopyStatus, so it is logged here rather than merged into UnattendedInstallStatus.
	//
	// Note this call is unbounded, unlike the META pair: if the cache is enabled and the copy
	// stalls, the reboot below is never requested. That is pre-existing behaviour (it was the
	// final statement of the install path before this split) and is left alone, since a
	// suitable timeout for a legitimate cache copy is a policy decision. With the cache
	// disabled CopyStatus is Skipped and this returns immediately.
	// No nil guard, deliberately: an unset hook must fail loudly in tests rather than
	// silently skip the wait.
	if err = ctrl.ImageCacheWaitFunc(ctx); err != nil {
		// Logged, not merged into postInstallErr: a stalled cache copy must not be
		// reportable as a META merge failure.
		logger.Error("image cache copy wait failed; the install itself is complete",
			zap.Error(err))
	}

	logger.Info("install successful")

	if ctrl.shouldReboot(doc) {
		logger.Info("requesting reboot after successful install")

		if err = safe.WriterModify(ctx, r, runtime.NewRebootRequest(), func(_ *runtime.RebootRequest) error {
			return nil
		}); err != nil {
			return fmt.Errorf("failed to create reboot request: %w", err)
		}
	} else {
		logger.Info("not rebooting after successful install (reboot disabled)")
	}

	return ctrl.setStatus(ctx, r, doc, ctrl.installedPhase(doc), ctrl.postInstallErr)
}

// installedPhase returns the phase to report once the install is complete for this boot.
//
// If the node is going to reboot after the install, the phase stays `waiting-for-reboot` until the
// reboot actually happens: reporting `installed` would open up the APIs gated on this phase (e.g.
// bootstrap) in the window before the reboot, and any such action would be lost on reboot.
func (ctrl *UnattendedInstallController) installedPhase(doc talosconfig.UnattendedInstallConfig) runtime.UnattendedInstallPhase {
	if ctrl.installDone && ctrl.shouldReboot(doc) {
		return runtime.UnattendedInstallPhaseWaitingForReboot
	}

	return runtime.UnattendedInstallPhaseInstalled
}

func (ctrl *UnattendedInstallController) getInstallerFromBootEntry(ctx context.Context, r controller.Runtime) (string, error) {
	schematic, err := safe.ReaderGetByID[*runtime.ImageFactorySchematic](ctx, r, runtime.ImageFactorySchematicID)
	if err != nil {
		return "", fmt.Errorf("failed to get image factory schematic: %w", err)
	}

	apiURL, err := url.Parse(schematic.TypedSpec().APIURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse image factory API URL: %w", err)
	}

	return images.NewInstallerImage(
		apiURL.Host,
		strings.ToLower(ctrl.PlatformFunc()),
		schematic.TypedSpec().SchematicID,
		"", // automatic fallback to current version if not specified
	), nil
}

func (ctrl *UnattendedInstallController) setStatus(
	ctx context.Context,
	r controller.Runtime,
	doc talosconfig.UnattendedInstallConfig,
	phase runtime.UnattendedInstallPhase,
	statusErr error,
) error {
	if statusErr == nil {
		// a caller with nothing to report must not erase a post-install failure
		statusErr = ctrl.postInstallErr
	}

	return safe.WriterModify(ctx, r, runtime.NewUnattendedInstallStatus(), func(status *runtime.UnattendedInstallStatus) error {
		status.TypedSpec().Image = doc.InstallerImage()
		status.TypedSpec().Phase = phase

		if statusErr != nil {
			status.TypedSpec().Error = statusErr.Error()
		} else {
			status.TypedSpec().Error = ""
		}

		return nil
	})
}

// shouldReboot determines whether the node should reboot after a successful install.
//
// The reboot behavior is controlled by the UnattendedInstallConfig.RebootAfterInstall():
//   - nil (not set): reboot only if an explicit installer image was provided
//   - true: always reboot
//   - false: never reboot
func (ctrl *UnattendedInstallController) shouldReboot(doc talosconfig.UnattendedInstallConfig) bool {
	switch reboot := doc.RebootAfterInstall(); reboot {
	case nil:
		return doc.InstallerImage() != ""
	default:
		return *reboot
	}
}
