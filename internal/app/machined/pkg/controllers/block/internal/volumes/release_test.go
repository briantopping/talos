// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package volumes_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/block/internal/volumes"
	"github.com/siderolabs/talos/internal/pkg/lvm"
)

type fakeLVM struct {
	pvs         []lvm.PV
	failVG      string
	deactivated []string
	listed      int
}

func (f *fakeLVM) PVS(context.Context) ([]lvm.PV, error) {
	f.listed++

	return f.pvs, nil
}

func (f *fakeLVM) VGChangeDeactivate(_ context.Context, vg string) error {
	if vg == f.failVG {
		return errors.New("Logical volume " + vg + "/ceph-osd in use")
	}

	f.deactivated = append(f.deactivated, vg)

	return nil
}

// The device under test is 252:2, the decrypted LUKS volume that VG "ssd" sits on.
func deviceAt(path string) (uint32, uint32, error) {
	if path == "/dev/mapper/luks2-r-lvm-data" {
		return 252, 2, nil
	}

	return 0, 0, fs.ErrNotExist
}

func TestReleaseVolumeGroups(t *testing.T) {
	defer volumes.SetDeviceNumber(deviceAt)()

	ctx := t.Context()
	logger := zaptest.NewLogger(t)
	device := "/dev/mapper/luks2-r-lvm-data"

	t.Run("the group on the device is deactivated once, others are left alone", func(t *testing.T) {
		f := &fakeLVM{pvs: []lvm.PV{
			{Device: "/dev/mapper/luks2-r-lvm-data", VGName: "ssd", Major: "252", Minor: "2"},
			{Device: "/dev/dm-2", VGName: "ssd", Major: "252", Minor: "2"},
			{Device: "/dev/sdc", VGName: "other", Major: "8", Minor: "32"},
			{Device: "/dev/sdd", Major: "8", Minor: "48"},
			{Device: "/dev/mapper/x", VGName: "minor-only", Major: "253", Minor: "2"},
		}}

		require.NoError(t, volumes.ReleaseVolumeGroups(ctx, logger, f, device))
		assert.Equal(t, []string{"ssd"}, f.deactivated)
	})

	t.Run("an open logical volume is an error, so the close is retried", func(t *testing.T) {
		f := &fakeLVM{pvs: []lvm.PV{{VGName: "ssd", Major: "252", Minor: "2"}}, failVG: "ssd"}

		err := volumes.ReleaseVolumeGroups(ctx, logger, f, device)
		require.Error(t, err)
		assert.ErrorContains(t, err, "in use")
		assert.Empty(t, f.deactivated)
	})

	t.Run("a device that is already gone needs nothing", func(t *testing.T) {
		f := &fakeLVM{pvs: []lvm.PV{{VGName: "ssd", Major: "252", Minor: "2"}}}

		require.NoError(t, volumes.ReleaseVolumeGroups(ctx, logger, f, "/dev/mapper/absent"))
		assert.Zero(t, f.listed)
		assert.Empty(t, f.deactivated)
	})

	t.Run("no LVM handle skips the step", func(t *testing.T) {
		require.NoError(t, volumes.ReleaseVolumeGroups(ctx, logger, nil, device))
	})
}
