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
	// Disk carries the ESP; the entry is expressed in that disk's sectors.
	Disk *blkid.Info
	// Part is the ESP's entry in that disk's partition table.
	Part blkid.NestedProbeResult
	// Suffix distinguishes siblings when a mirrored ESP yields one entry per member; empty for a single ESP, keeping its description unchanged.
	Suffix string
}

// Description is the UEFI boot entry description for this target.
func (t BootTarget) Description() string {
	if t.Suffix == "" {
		return TalosBootEntryDescription
	}

	return TalosBootEntryDescription + " (" + t.Suffix + ")"
}

// isTalosBootEntry reports whether a description is one of ours. Prefix matching lets a machine move between a single and a mirrored ESP without leaving orphans.
func isTalosBootEntry(description string) bool {
	return description == TalosBootEntryDescription ||
		strings.HasPrefix(description, TalosBootEntryDescription+" (")
}

// FindESPPartition returns the ESP entry for a member partition. Members are labeled by the volume manager, not "EFI", so they are found by node and type.
func FindESPPartition(disk *blkid.Info, diskPath, member string) (blkid.NestedProbeResult, error) {
	for _, part := range disk.Parts {
		if filepath.Base(partitioning.DevName(diskPath, part.PartitionIndex)) != member {
			continue
		}

		if part.PartitionType == nil || !strings.EqualFold(part.PartitionType.String(), partition.EFISystemPartition) {
			return blkid.NestedProbeResult{}, fmt.Errorf("partition %s is not an EFI System Partition", member)
		}

		return part, nil
	}

	return blkid.NestedProbeResult{}, fmt.Errorf("partition %s not found on disk %s", member, diskPath)
}

// ESPTargetsFromDisk returns the single boot target for a disk carrying an ESP partition labeled EFI, which is the ordinary non-mirrored install.
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

// CreateBootEntries creates one UEFI boot entry per target and removes stale ones.
// A mirrored ESP yields one target per member: firmware boots a partition, not an
// array, so naming a single member leaves the machine unbootable if that disk fails.
//
// CreateBootEntries writes one UEFI boot entry per target and orders them first.
//
// mirrored means the ESP is an md array, so targets are the members present now: a degraded
// array yields fewer targets, and surplus entries are kept rather than deleted.
//
//nolint:gocyclo,cyclop
func CreateBootEntries(rw efivarfs.ReadWriter, targets []BootTarget, mirrored bool, printf func(format string, args ...any), sdBootFilePath string) error {
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

	// keep the lowest indexes so a reinstall reuses its existing slots
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

	// see https://github.com/siderolabs/talos/issues/11829
	for _, idx := range existingTalosBootEntryIndexes {
		if slices.Contains(indexes, idx) {
			continue
		}

		// on a mirror a surplus entry is an absent member, not a stale installation
		if mirrored {
			printf("Keeping existing Talos Linux UKI boot entry at index %d: the ESP is mirrored and this entry may name a member that is currently absent", idx)

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

	// order every member's entry first: an entry which exists but sits behind removable
	// media is never reached
	newBootOrder := make(efivarfs.BootOrder, 0, len(indexes)+len(bootOrder))

	for _, idx := range indexes {
		newBootOrder = append(newBootOrder, uint16(idx))
	}

	newBootOrder = efivarfs.UniqueBootOrder(append(newBootOrder, bootOrder...))

	if err := efivarfs.SetBootOrder(rw, newBootOrder); err != nil {
		return fmt.Errorf("failed to set BootOrder: %w", err)
	}

	printf("BootOrder set to %v", newBootOrder)

	return nil
}
