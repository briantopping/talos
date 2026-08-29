// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sdboot

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/siderolabs/gen/xslices"
	"github.com/siderolabs/go-blockdevice/v2/blkid"
	"github.com/siderolabs/go-blockdevice/v2/partitioning"

	"github.com/siderolabs/talos/internal/pkg/efivarfs"
	"github.com/siderolabs/talos/internal/pkg/partition"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// TalosBootEntryDescription is the description of the Talos Linux UKI UEFI boot entry.
const TalosBootEntryDescription = "Talos Linux UKI"

// SystemdBootStubInfoPath is the path to the SystemdBoot StubInfo EFI variable.
var SystemdBootStubInfoPath = constants.EFIVarsMountPoint + "/" + "StubInfo-" + efivarfs.ScopeSystemd.String()

// Variable names.
const (
	LoaderConfigTimeoutName     = "LoaderConfigTimeout"
	LoaderEntryDefaultName      = "LoaderEntryDefault"
	LoaderEntryOneShotName      = "LoaderEntryOneShot"
	LoaderEntryRebootReasonName = "LoaderEntryRebootReason"
	LoaderEntrySelectedName     = "LoaderEntrySelected"

	StubImageIdentifierName = "StubImageIdentifier"
)

// ReadVariable reads a SystemdBoot EFI variable.
func ReadVariable(name string) (string, error) {
	efi, err := efivarfs.NewFilesystemReaderWriter(false)
	if err != nil {
		return "", fmt.Errorf("failed to create efivarfs reader/writer: %w", err)
	}

	defer efi.Close() //nolint:errcheck

	data, _, err := efi.Read(efivarfs.ScopeSystemd, name)
	if err != nil {
		// if the variable does not exist, return an empty string
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", err
	}

	out := make([]byte, len(data))

	decoder := efivarfs.Encoding.NewDecoder()

	n, _, err := decoder.Transform(out, data, true)
	if err != nil {
		return "", err
	}

	if n > 0 && out[n-1] == 0 {
		n--
	}

	return string(out[:n]), nil
}

// WriteVariable reads a SystemdBoot EFI variable.
func WriteVariable(name, value string) error {
	efi, err := efivarfs.NewFilesystemReaderWriter(true)
	if err != nil {
		return fmt.Errorf("failed to create efivarfs reader/writer: %w", err)
	}

	defer efi.Close() //nolint:errcheck

	out := make([]byte, (len(value)+1)*2)

	encoder := efivarfs.Encoding.NewEncoder()

	n, _, err := encoder.Transform(out, []byte(value), true)
	if err != nil {
		return err
	}

	out = append(out[:n], 0, 0)

	return efi.Write(efivarfs.ScopeSystemd, name, efivarfs.AttrBootserviceAccess|efivarfs.AttrRuntimeAccess|efivarfs.AttrNonVolatile, out)
}

// BootTarget is one EFI System Partition a UEFI boot entry should point at.
type BootTarget struct {
	// Disk is the probe of the DISK carrying the ESP; the boot entry is expressed in
	// that disk's sectors, so it cannot be derived from the partition alone.
	Disk *blkid.Info
	// Part is the ESP's entry in that disk's partition table.
	Part blkid.NestedProbeResult
	// Suffix distinguishes this entry from its siblings when a mirrored ESP
	// produces one entry per member. Empty for an ordinary single ESP, which keeps
	// that entry's description byte-identical to what earlier versions wrote.
	Suffix string
}

// Description is the UEFI boot entry description for this target.
func (t BootTarget) Description() string {
	if t.Suffix == "" {
		return TalosBootEntryDescription
	}

	return TalosBootEntryDescription + " (" + t.Suffix + ")"
}

// isTalosBootEntry reports whether a boot entry description is one of ours.
//
// Matching a PREFIX rather than the exact string is what lets a machine move
// between a single ESP and a mirrored one without accumulating orphans: the
// entries written for a mirror are named per member, and a later install with one
// ESP has to be able to clean them up.
func isTalosBootEntry(description string) bool {
	return description == TalosBootEntryDescription ||
		strings.HasPrefix(description, TalosBootEntryDescription+" (")
}

// FindESPPartition returns the ESP entry for a member partition of a disk.
//
// A mirrored ESP's members are labelled by the volume manager that created them
// (r-esp-a, r-esp-b), NOT "EFI" -- so they cannot be found the way a single ESP is
// found, by label. They are identified by being the partition at that device node,
// and confirmed by their partition TYPE.
func FindESPPartition(disk *blkid.Info, diskPath, member string) (blkid.NestedProbeResult, error) {
	for _, part := range disk.Parts {
		if filepath.Base(partitioning.DevName(diskPath, uint(part.PartitionIndex))) != member {
			continue
		}

		if part.PartitionType == nil || !strings.EqualFold(part.PartitionType.String(), partition.EFISystemPartition) {
			return blkid.NestedProbeResult{}, fmt.Errorf("partition %s is not an EFI System Partition", member)
		}

		return part, nil
	}

	return blkid.NestedProbeResult{}, fmt.Errorf("partition %s not found on disk %s", member, diskPath)
}

// ESPTargetsFromDisk returns the single boot target for a disk carrying an ESP
// partition labelled EFI, which is the ordinary non-mirrored install.
func ESPTargetsFromDisk(blkidInfo *blkid.Info) ([]BootTarget, error) {
	efiPartInfo := xslices.Filter(blkidInfo.Parts, func(part blkid.NestedProbeResult) bool {
		return part.PartitionLabel != nil && *part.PartitionLabel == constants.EFIPartitionLabel
	})

	if len(efiPartInfo) == 0 {
		return nil, fmt.Errorf("EFI partition not found on install disk %q", blkidInfo.Name)
	}

	if len(efiPartInfo) > 1 {
		return nil, fmt.Errorf("multiple EFI partitions found on install disk %q, expected only one", blkidInfo.Name)
	}

	return []BootTarget{{Disk: blkidInfo, Part: efiPartInfo[0]}}, nil
}

// CreateBootEntries creates a UEFI boot entry per target, each named "Talos Linux UKI"
// and pointing at that target's ESP, and removes any of ours which are left over.
//
// There is more than one target when the ESP is a mirror. Firmware knows nothing
// about md: it boots a PARTITION on a physical disk. So an entry pointing at the
// array would name a device the firmware cannot see, and an entry pointing at only
// one member leaves the machine unbootable the day that disk is the one that fails
// -- which is the entire reason for mirroring the ESP. One entry per member is what
// makes a degraded boot work, and it is only possible because metadata 1.0 keeps
// each member a valid FAT ESP in its own right.
//
//nolint:gocyclo,cyclop
func CreateBootEntries(rw efivarfs.ReadWriter, targets []BootTarget, printf func(format string, args ...any), sdBootFilePath string) error {
	if len(targets) == 0 {
		return errors.New("no EFI System Partition to create a boot entry for")
	}

	for _, target := range targets {
		if target.Part.PartitionUUID == nil {
			return fmt.Errorf("EFI partition UUID not found on disk %q", target.Disk.Name)
		}

		printf("using disk %s with partition %d and UUID %s", target.Disk.Name, target.Part.PartitionIndex, target.Part.PartitionUUID.String())
	}

	bootOrder, err := efivarfs.GetBootOrder(rw)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			bootOrder = efivarfs.BootOrder{}
		} else {
			return fmt.Errorf("failed to get BootOrder: %w", err)
		}
	}

	printf("Current BootOrder: %v", bootOrder)

	bootEntries, err := efivarfs.ListBootEntries(rw)
	if err != nil {
		return fmt.Errorf("failed to list existing Talos boot entries: %w", err)
	}

	printf("Existing boot entries: %v", slices.Collect(maps.Keys(bootEntries)))

	var existingTalosBootEntryIndexes []int

	for idx, entry := range bootEntries {
		if isTalosBootEntry(entry.Description) {
			existingTalosBootEntryIndexes = append(existingTalosBootEntryIndexes, idx)
		}
	}

	// keep the lowest indexes, so a reinstall reuses the slots it already had rather
	// than walking up the variable space on every install
	slices.Sort(existingTalosBootEntryIndexes)

	printf("Found existing Talos Linux UKI boot entries: %v", existingTalosBootEntryIndexes)

	// reuse one existing index per target, then allocate for whatever is left over
	indexes := make([]int, 0, len(targets))

	for i := range targets {
		if i < len(existingTalosBootEntryIndexes) {
			indexes = append(indexes, existingTalosBootEntryIndexes[i])

			continue
		}

		next := -1

		for candidate := range math.MaxUint16 {
			if _, taken := bootEntries[candidate]; taken {
				continue
			}

			if slices.Contains(indexes, candidate) {
				continue
			}

			next = candidate

			break
		}

		if next == -1 {
			return errors.New("all 2^16 boot entry variables are occupied")
		}

		indexes = append(indexes, next)
	}

	// Talos 1.11.x assumed the boot order it set survived a reboot, but firmware may
	// reorder on boot, which produced duplicate Talos entries in the boot order and
	// some firmwares then failed to boot at all. See
	// https://github.com/siderolabs/talos/issues/11829
	for _, idx := range existingTalosBootEntryIndexes {
		if slices.Contains(indexes, idx) {
			continue
		}

		printf("Removing existing Talos Linux UKI boot entry at index %d", idx)

		if err := efivarfs.DeleteBootEntry(rw, idx); err != nil {
			return fmt.Errorf("failed to delete existing Talos boot entry at index %d: %w", idx, err)
		}
	}

	for i, target := range targets {
		if err := efivarfs.SetBootEntry(rw, indexes[i], &efivarfs.LoadOption{
			Description: target.Description(),
			FilePath: efivarfs.DevicePath{
				&efivarfs.HardDrivePath{
					PartitionNumber:     uint32(target.Part.PartitionIndex),
					PartitionStartBlock: target.Part.PartitionOffset / uint64(target.Disk.SectorSize),
					PartitionSizeBlocks: target.Part.PartitionSize / uint64(target.Disk.SectorSize),
					PartitionMatch: &efivarfs.PartitionGPT{
						PartitionUUID: *target.Part.PartitionUUID,
					},
				},
				efivarfs.FilePath("/" + sdBootFilePath),
			},
		}); err != nil {
			return fmt.Errorf("failed to create %q boot entry at index %d: %w", target.Description(), indexes[i], err)
		}

		printf("created %q boot entry at index %d", target.Description(), indexes[i])
	}

	return nil
}
