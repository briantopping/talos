// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/siderolabs/go-blockdevice/v2/blkid"
	"github.com/siderolabs/go-blockdevice/v2/partitioning"

	bootloaderoptions "github.com/siderolabs/talos/internal/app/machined/pkg/runtime/v1alpha1/bootloader/options"
	"github.com/siderolabs/talos/internal/pkg/partition"
	"github.com/siderolabs/talos/pkg/machinery/constants"
)

// DiscoverMirroredESP returns the md array whose members all carry the ESP type, or "" when there is not exactly one.
//
//nolint:gocyclo
func DiscoverMirroredESP(probeOptions ...blkid.ProbeOption) (string, []bootloaderoptions.ESPMember, error) {
	sysBlock, err := os.ReadDir("/sys/block")
	if err != nil {
		return "", nil, fmt.Errorf("error reading /sys/block: %w", err)
	}

	var (
		found   []string
		members [][]bootloaderoptions.ESPMember
	)

	for _, entry := range sysBlock {
		if !strings.HasPrefix(entry.Name(), "md") {
			continue
		}

		slaves, err := os.ReadDir(filepath.Join("/sys/block", entry.Name(), "slaves"))
		if err != nil || len(slaves) == 0 {
			continue
		}

		allESP := true

		var arrayMembers []bootloaderoptions.ESPMember

		for _, slave := range slaves {
			parent, isESP, err := memberIsESP(slave.Name(), probeOptions...)
			if err != nil || !isESP {
				allESP = false

				break
			}

			arrayMembers = append(arrayMembers, bootloaderoptions.ESPMember{Disk: parent, Partition: slave.Name()})
		}

		if allESP {
			found = append(found, filepath.Join("/dev", entry.Name()))
			members = append(members, arrayMembers)
		}
	}

	switch len(found) {
	case 0:
		return "", nil, nil
	case 1:
		return found[0], members[0], nil
	default:
		return "", nil, fmt.Errorf("found %d md arrays whose members are EFI System Partitions (%s); "+
			"refusing to choose one to install the bootloader into", len(found), strings.Join(found, ", "))
	}
}

// memberIsESP reports whether a partition carries the EFI System Partition type, read from its parent disk's partition table.
func memberIsESP(member string, probeOptions ...blkid.ProbeOption) (string, bool, error) {
	// a partition's sysfs dir sits under its parent's; a whole disk cannot be an ESP
	parentLink, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", member))
	if err != nil {
		return "", false, err
	}

	parent := filepath.Base(filepath.Dir(parentLink))
	if parent == "block" {
		return "", false, nil
	}

	parentPath := filepath.Join("/dev", parent)

	info, err := blkid.ProbePath(parentPath, probeOptions...)
	if err != nil {
		return "", false, err
	}

	for _, part := range info.Parts {
		if filepath.Base(partitioning.DevName(parentPath, part.PartitionIndex)) != member {
			continue
		}

		return parentPath, part.PartitionType != nil &&
			strings.EqualFold(part.PartitionType.String(), partition.EFISystemPartition), nil
	}

	return "", false, nil
}

// EnsureESPFilesystem formats a mirrored ESP that has never been formatted. An array that already carries a filesystem is left alone.
func EnsureESPFilesystem(ctx context.Context, dev string, talosVersion string, printf func(string, ...any)) error {
	info, err := blkid.ProbePath(dev)
	if err != nil {
		return fmt.Errorf("error probing %s: %w", dev, err)
	}

	if info.Name != "" {
		return nil
	}

	printf("formatting mirrored ESP %s as %s", dev, partition.FilesystemTypeVFAT)

	return partition.Format(ctx, dev, &partition.FormatOptions{
		Label:          constants.EFIPartitionLabel,
		FileSystemType: partition.FilesystemTypeVFAT,
	}, talosVersion, printf)
}
