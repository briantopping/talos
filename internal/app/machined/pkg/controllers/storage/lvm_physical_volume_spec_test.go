// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"github.com/siderolabs/talos/internal/app/machined/pkg/controllers/ctest"
	storagectrl "github.com/siderolabs/talos/internal/app/machined/pkg/controllers/storage"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	storageres "github.com/siderolabs/talos/pkg/machinery/resources/storage"
)

type LVMPhysicalVolumeSpecSuite struct {
	ctest.DefaultSuite
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSelectsMatchingDisks() {
	createDisk(&suite.DefaultSuite, "sda", "/dev/sda", "sata")
	createDisk(&suite.DefaultSuite, "nvme0n1", "/dev/nvme0n1", "nvme")
	createDisk(&suite.DefaultSuite, "nvme1n1", "/dev/nvme1n1", "nvme")

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `disk.transport == "nvme"`))

	ctest.AssertResources(
		suite,
		[]string{"nvme0n1", "nvme1n1"},
		func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
			asrt.Equal("vg-pool", pv.TypedSpec().VGName)
		},
	)

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "sda")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestEmptyConfigEmitsNothing() {
	createDisk(&suite.DefaultSuite, "nvme0n1", "/dev/nvme0n1", "nvme")

	applyMachineConfig(&suite.DefaultSuite)

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "nvme0n1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestRemovingConfigCleansSpecs() {
	createDisk(&suite.DefaultSuite, "nvme0n1", "/dev/nvme0n1", "nvme")

	cfg := applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `disk.transport == "nvme"`))

	ctest.AssertResources(
		suite,
		[]string{"nvme0n1"},
		func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
			asrt.Equal("/dev/nvme0n1", pv.TypedSpec().Device)
		},
	)

	suite.Destroy(cfg)

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "nvme0n1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSelectsPartitionByLabel() {
	// Whole disk plus a raw-volume partition on it.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `volume.partition_label == "r-lvmpv0"`))

	ctest.AssertResource(suite, "vdb1", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb1", pv.TypedSpec().Device)
		asrt.Equal("vg-pool", pv.TypedSpec().VGName)
	})

	// The disk-level selector must not also claim the whole parent disk.
	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSelectsPartitionsByLabelPrefix() {
	// Mirrors the documented example selector
	// `volume.partition_label.startsWith("r-lvm")`.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	createPartition(&suite.DefaultSuite, "vdb2", "/dev/vdb2", "/dev/vdb", "r-lvmpv1")
	// A partition that should NOT match the prefix.
	createPartition(&suite.DefaultSuite, "vdb3", "/dev/vdb3", "/dev/vdb", "r-data0")

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `volume.partition_label.startsWith("r-lvm")`))

	ctest.AssertResources(
		suite,
		[]string{"vdb1", "vdb2"},
		func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
			asrt.Equal("vg-pool", pv.TypedSpec().VGName)
		},
	)

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb3")
	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSkipsPartitionedDisk() {
	// A whole disk that carries partitions cannot be a PV; pvcreate rejects it.
	// The selector matches the whole disk (by transport) and a raw-volume
	// partition (by label); only the partition must yield a PV spec.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-data0")
	// A whole, unpartitioned disk that should still be used directly.
	createDisk(&suite.DefaultSuite, "vdd", "/dev/vdd", "virtio")

	applyMachineConfig(&suite.DefaultSuite,
		newVGDoc("vg-pool", `disk.transport == "virtio" || volume.partition_label.startsWith("r-data")`),
	)

	ctest.AssertResources(
		suite,
		[]string{"vdb1", "vdd"},
		func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
			asrt.Equal("vg-pool", pv.TypedSpec().VGName)
		},
	)

	// The partitioned whole disk must not become a PV.
	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestDiskSelectorMatchesWholeDiskOnly() {
	// A disk-level predicate matches only the whole disk, never its partitions:
	// partitions get an empty disk in the CEL context so disk.* evaluates false.
	// vdb is whole and unpartitioned; vdc carries a partition vdc1.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createDisk(&suite.DefaultSuite, "vdc", "/dev/vdc", "virtio")
	createPartition(&suite.DefaultSuite, "vdc1", "/dev/vdc1", "/dev/vdc", "r-lvmpv0")

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `disk.dev_path == "/dev/vdb"`))

	ctest.AssertResource(suite, "vdb", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("vg-pool", pv.TypedSpec().VGName)
	})

	// A partition is never claimed by a disk-level selector.
	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdc1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestOverlappingVGsSurfaceValidationError() {
	createDisk(&suite.DefaultSuite, "nvme0n1", "/dev/nvme0n1", "nvme")

	applyMachineConfig(
		&suite.DefaultSuite,
		newVGDoc("vg-a", `disk.transport == "nvme"`),
		newVGDoc("vg-b", `disk.transport == "nvme"`),
	)

	// First VG (by config order) wins the device.
	ctest.AssertResource(suite, "nvme0n1", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("vg-a", pv.TypedSpec().VGName)
	})

	// The losing VG gets a validation error surfaced.
	ctest.AssertResource(suite, "vg-b", func(e *storageres.LVMValidationError, asrt *assert.Assertions) {
		asrt.Equal("vg-b", e.TypedSpec().VGName)
		asrt.Contains(e.TypedSpec().Message, "vg-a")
	})

	ctest.AssertNoResource[*storageres.LVMValidationError](suite, "vg-a")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestMultipleVGsDistinctDisks() {
	createDisk(&suite.DefaultSuite, "nvme0n1", "/dev/nvme0n1", "nvme")
	createDisk(&suite.DefaultSuite, "sda", "/dev/sda", "sata")

	applyMachineConfig(
		&suite.DefaultSuite,
		newVGDoc("vg-nvme", `disk.transport == "nvme"`),
		newVGDoc("vg-sata", `disk.transport == "sata"`),
	)

	ctest.AssertResource(suite, "nvme0n1", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("vg-nvme", pv.TypedSpec().VGName)
	})

	ctest.AssertResource(suite, "sda", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("vg-sata", pv.TypedSpec().VGName)
	})
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSelectsDecryptedDeviceForEncryptedVolume() {
	// A raw volume partition that carries a LUKS2 container. The selector
	// matches the GPT partition label (the stable, user-authored identifier),
	// but the PV must be created on the OPENED device: pvcreate against the
	// ciphertext either aborts on the crypto_LUKS signature or, with --yes,
	// would destroy the LUKS header.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	createVolumeStatus(
		&suite.DefaultSuite, "lvmpv0",
		block.VolumePhaseReady, block.EncryptionProviderLUKS2,
		"/dev/vdb1", "/dev/dm-0",
	)

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `volume.partition_label == "r-lvmpv0"`))

	ctest.AssertResource(suite, "dm-0", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/dm-0", pv.TypedSpec().Device)
		asrt.Equal("vg-pool", pv.TypedSpec().VGName)
	})

	// The ciphertext partition must never be handed to pvcreate.
	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSkipsEncryptedVolumeNotYetOpen() {
	// The LUKS container has not been opened yet (no MountLocation). Until it
	// is, there is no device to make a PV out of, and the ciphertext partition
	// is not a substitute.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	// A plain partition matched by the same selector: it is the positive
	// control. Waiting for its PV proves the controller reconciled, so the
	// absence assertions below are not merely observed too early.
	createPartition(&suite.DefaultSuite, "vdb2", "/dev/vdb2", "/dev/vdb", "r-lvmpv1")
	// NOTE the provider is None, not LUKS2: HandleEncryption assigns
	// Status.EncryptionProvider only AFTER a successful open, so this is what a
	// still-locked encrypted volume actually reports. A fixture using LUKS2 here
	// would describe a state the volume manager never produces, and would pass
	// against a controller that keys its decision off the provider.
	createVolumeStatus(
		&suite.DefaultSuite, "lvmpv0",
		block.VolumePhaseWaiting, block.EncryptionProviderNone,
		"/dev/vdb1", "",
	)

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `volume.partition_label.startsWith("r-lvm")`))

	ctest.AssertResource(suite, "vdb2", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb2", pv.TypedSpec().Device)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb1")
	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "dm-0")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestUnencryptedVolumeStatusDoesNotRedirect() {
	// HandleEncryption sets MountLocation == Location for unencrypted volumes,
	// so a VolumeStatus being present must not change the device selection.
	// This is the no-regression control for the redirect above.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	createVolumeStatus(
		&suite.DefaultSuite, "lvmpv0",
		block.VolumePhaseReady, block.EncryptionProviderNone,
		"/dev/vdb1", "/dev/vdb1",
	)

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `volume.partition_label == "r-lvmpv0"`))

	ctest.AssertResource(suite, "vdb1", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb1", pv.TypedSpec().Device)
	})
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSkipsConfiguredEncryptedVolumeBeforeStatusLocation() {
	// Startup ordering: the machine config, discovered volume and volume status
	// controllers are not ordered against each other. A freshly created VolumeStatus
	// is Waiting with an empty Location, so nothing about the status yet says this
	// device is a LUKS container. The config does, and it says so immediately.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	// Positive control: a plain partition matched by the same selector. Waiting for
	// its PV proves the controller reconciled, so the absence below is a real absence.
	createPartition(&suite.DefaultSuite, "vdb2", "/dev/vdb2", "/dev/vdb", "r-plain0")
	createVolumeStatus(
		&suite.DefaultSuite, "lvmpv0",
		block.VolumePhaseWaiting, block.EncryptionProviderNone,
		"", "",
	)

	applyMachineConfigDocs(&suite.DefaultSuite,
		newEncryptedRawVolumeDoc("lvmpv0"),
		newVGDoc("vg-pool", `volume.partition_label.startsWith("r-")`),
	)

	ctest.AssertResource(suite, "vdb2", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb2", pv.TypedSpec().Device)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSkipsConfiguredEncryptedVolumeWithNoStatus() {
	// A configured encrypted volume that has NEVER had a VolumeStatus: the
	// status controller has not run yet. Absence of a status must not read as
	// "plain device". The destroy-after-resolve sequence is covered separately
	// by TestSkipsEncryptedVolumeAfterStatusDestroyed.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	createPartition(&suite.DefaultSuite, "vdb2", "/dev/vdb2", "/dev/vdb", "r-plain0")

	applyMachineConfigDocs(&suite.DefaultSuite,
		newEncryptedRawVolumeDoc("lvmpv0"),
		newVGDoc("vg-pool", `volume.partition_label.startsWith("r-")`),
	)

	ctest.AssertResource(suite, "vdb2", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb2", pv.TypedSpec().Device)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb1")
}

// TestSkipsEncryptedVolumeAfterStatusDestroyed covers the teardown window: the
// volume resolves normally, then its VolumeStatus is destroyed while the
// ciphertext DiscoveredVolume still exists. The controller must not fall back
// to emitting the ciphertext device once the status it was resolving through
// disappears. This exercises the DELETION SEQUENCE, not merely the end state --
// a controller that only ever saw a missing status would pass without it.
func (suite *LVMPhysicalVolumeSpecSuite) TestSkipsEncryptedVolumeAfterStatusDestroyed() {
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	createPartition(&suite.DefaultSuite, "vdb2", "/dev/vdb2", "/dev/vdb", "r-plain0")
	createDisk(&suite.DefaultSuite, "dm-0", "/dev/dm-0", "")

	createVolumeStatus(&suite.DefaultSuite, "r-lvmpv0", block.VolumePhaseReady,
		block.EncryptionProviderLUKS2, "/dev/vdb1", "/dev/dm-0")

	applyMachineConfigDocs(&suite.DefaultSuite,
		newEncryptedRawVolumeDoc("lvmpv0"),
		newVGDoc("vg-pool", `volume.partition_label.startsWith("r-")`),
	)

	// It resolves to the opened device first, so the destroy below is a real
	// transition rather than a state the controller was always in.
	ctest.AssertResource(suite, "dm-0", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/dm-0", pv.TypedSpec().Device)
	})

	vs := block.NewVolumeStatus(block.NamespaceName, "r-lvmpv0")
	suite.Destroy(vs)

	// The barrier has to be satisfiable ONLY AFTER the destroy, or it proves
	// nothing: vdb2 was already emitted above, so re-asserting it would pass
	// without the controller ever re-running. This partition appears now, so
	// its PV can only exist if reconciliation happened after the status went
	// away -- which is the window under test.
	createPartition(&suite.DefaultSuite, "vdb3", "/dev/vdb3", "/dev/vdb", "r-plain1")

	ctest.AssertResource(suite, "vdb3", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb3", pv.TypedSpec().Device)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestWaitsForVolumeManagerOnUnencryptedVolume() {
	// Deliberate consequence of keying on MountLocation: a volume the manager has
	// LOCATED but not yet PREPARED has Location set and MountLocation still empty
	// (locate.go sets Location at Located/Provisioned; HandleEncryption sets
	// MountLocation at Prepared). Such a device is not offered to pvcreate yet even
	// though it is not encrypted -- the volume manager has not finished with it.
	//
	// Pinned as a test because it is a behavior change beyond the encryption case,
	// and should be noticed if anyone narrows the rule later.
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")
	createPartition(&suite.DefaultSuite, "vdb1", "/dev/vdb1", "/dev/vdb", "r-lvmpv0")
	createPartition(&suite.DefaultSuite, "vdb2", "/dev/vdb2", "/dev/vdb", "r-plain0")
	createVolumeStatus(
		&suite.DefaultSuite, "lvmpv0",
		block.VolumePhaseLocated, block.EncryptionProviderNone,
		"/dev/vdb1", "",
	)

	applyMachineConfig(&suite.DefaultSuite, newVGDoc("vg-pool", `volume.partition_label.startsWith("r-")`))

	// Barrier: the partition with no VolumeStatus at all is unaffected.
	ctest.AssertResource(suite, "vdb2", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb2", pv.TypedSpec().Device)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "vdb1")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSkipsEncryptedWholeDiskVolumeBeforeStatus() {
	// The shape used to encrypt an MD array and hand it to LVM: a UserVolumeConfig
	// with volumeType: disk claims the ENTIRE device, so there is no partition and no
	// partition label. It can only be identified by the volume's own disk selector.
	//
	// Before the volume manager publishes a status there is nothing else to go on, and
	// a missing entry must not read as "plain device" -- mdadm's array is a block.Disk
	// like any other.
	createDisk(&suite.DefaultSuite, "md0", "/dev/md0", "")
	createDisk(&suite.DefaultSuite, "vdb", "/dev/vdb", "virtio")

	applyMachineConfigDocs(&suite.DefaultSuite,
		newEncryptedWholeDiskUserVolumeDoc("lvmdata", `disk.dev_path.startsWith("/dev/md")`),
		newVGDoc("vg-pool", `disk.dev_path.startsWith("/dev/md") || disk.transport == "virtio"`),
	)

	// Positive control: the plain disk still becomes a PV, proving reconciliation ran.
	ctest.AssertResource(suite, "vdb", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/vdb", pv.TypedSpec().Device)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "md0")
}

func (suite *LVMPhysicalVolumeSpecSuite) TestSelectsDecryptedDeviceForWholeDiskVolume() {
	// Once the array is open, the PV belongs on the opened device, not on /dev/md0.
	createDisk(&suite.DefaultSuite, "md0", "/dev/md0", "")
	createVolumeStatus(
		&suite.DefaultSuite, "u-lvmdata",
		block.VolumePhaseReady, block.EncryptionProviderLUKS2,
		"/dev/md0", "/dev/dm-0",
	)

	applyMachineConfigDocs(&suite.DefaultSuite,
		newEncryptedWholeDiskUserVolumeDoc("lvmdata", `disk.dev_path.startsWith("/dev/md")`),
		newVGDoc("vg-pool", `disk.dev_path.startsWith("/dev/md")`),
	)

	ctest.AssertResource(suite, "dm-0", func(pv *storageres.LVMPhysicalVolumeSpec, asrt *assert.Assertions) {
		asrt.Equal("/dev/dm-0", pv.TypedSpec().Device)
		asrt.Equal("vg-pool", pv.TypedSpec().VGName)
	})

	ctest.AssertNoResource[*storageres.LVMPhysicalVolumeSpec](suite, "md0")
}

func TestLVMPhysicalVolumeSpecSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, &LVMPhysicalVolumeSpecSuite{
		DefaultSuite: ctest.DefaultSuite{
			Timeout: 5 * time.Second,
			AfterSetup: func(s *ctest.DefaultSuite) {
				s.Require().NoError(s.Runtime().RegisterController(&storagectrl.LVMPhysicalVolumeSpecController{}))
			},
		},
	})
}
