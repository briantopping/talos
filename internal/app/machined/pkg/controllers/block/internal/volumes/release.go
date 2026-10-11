// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumes

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/siderolabs/talos/internal/pkg/lvm"
)

// LVM is the part of the LVM API needed to release a device before it is closed.
type LVM interface {
	PVS(ctx context.Context) ([]lvm.PV, error)
	VGChangeDeactivate(ctx context.Context, vgName string) error
}

// deviceNumber returns the major and minor numbers of a block device node.
var deviceNumber = func(path string) (uint32, uint32, error) {
	var st unix.Stat_t

	if err := unix.Stat(path, &st); err != nil {
		return 0, 0, err
	}

	return unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)), nil //nolint:unconvert
}

// ReleaseVolumeGroups deactivates every volume group with a physical volume on the device.
//
// An active logical volume holds the device beneath it open, so closing that device fails until the group is
// deactivated: on shutdown the encrypted volume under a Talos-managed volume group could never be closed.
// Deactivation fails while a logical volume is still open, which the caller retries.
func ReleaseVolumeGroups(ctx context.Context, logger *zap.Logger, l LVM, device string) error {
	if l == nil {
		return nil
	}

	major, minor, err := deviceNumber(device)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("error reading device number of %q: %w", device, err)
	}

	pvs, err := l.PVS(ctx)
	if err != nil {
		return fmt.Errorf("error listing physical volumes: %w", err)
	}

	wantMajor, wantMinor := strconv.FormatUint(uint64(major), 10), strconv.FormatUint(uint64(minor), 10)
	released := map[string]struct{}{}

	for _, pv := range pvs {
		if pv.VGName == "" || pv.Major != wantMajor || pv.Minor != wantMinor {
			continue
		}

		if _, ok := released[pv.VGName]; ok {
			continue
		}

		if err := l.VGChangeDeactivate(ctx, pv.VGName); err != nil {
			return fmt.Errorf("error deactivating volume group %q on %q: %w", pv.VGName, device, err)
		}

		released[pv.VGName] = struct{}{}

		logger.Info("volume group deactivated", zap.String("vg", pv.VGName), zap.String("device", device))
	}

	return nil
}
