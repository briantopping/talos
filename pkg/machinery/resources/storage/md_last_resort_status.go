// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package storage

import (
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"

	"github.com/siderolabs/talos/pkg/machinery/proto"
)

// MDLastResortStatusType is the type of MDLastResortStatus resource.
const MDLastResortStatusType = resource.Type("MDLastResortStatuses.storage.talos.dev")

// MDLastResortStatusID is the singleton ID of the MDLastResortStatus resource.
const MDLastResortStatusID = "md-last-resort"

// MDLastResortStatus reports whether last-resort assembly of degraded MD arrays has finished trying, so a missing volume can be told from a not-yet-assembled one.
type MDLastResortStatus = typed.Resource[MDLastResortStatusSpec, MDLastResortStatusExtension]

// MDLastResortStatusSpec is the spec for MDLastResortStatus resource.
//
//gotagsrewrite:gen
type MDLastResortStatusSpec struct {
	// Settled reports that no further arrays are expected to appear.
	Settled bool `yaml:"settled" protobuf:"1"`
	// Pending lists the inactive arrays still within the grace period.
	Pending []string `yaml:"pending,omitempty" protobuf:"2"`
}

// NewMDLastResortStatus initializes an MDLastResortStatus resource.
func NewMDLastResortStatus(namespace resource.Namespace, id resource.ID) *MDLastResortStatus {
	return typed.NewResource[MDLastResortStatusSpec, MDLastResortStatusExtension](
		resource.NewMetadata(namespace, MDLastResortStatusType, id, resource.VersionUndefined),
		MDLastResortStatusSpec{},
	)
}

// MDLastResortStatusExtension is auxiliary resource data for MDLastResortStatus.
type MDLastResortStatusExtension struct{}

// ResourceDefinition implements meta.ResourceDefinitionProvider interface.
func (MDLastResortStatusExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{
		Type:             MDLastResortStatusType,
		DefaultNamespace: NamespaceName,
		PrintColumns: []meta.PrintColumn{
			{Name: "Settled", JSONPath: "{.settled}"},
			{Name: "Pending", JSONPath: "{.pending}"},
		},
	}
}

func init() {
	proto.RegisterDefaultTypes()

	if err := protobuf.RegisterDynamic(MDLastResortStatusType, &MDLastResortStatus{}); err != nil {
		panic(err)
	}
}

// DeepCopy generates a deep copy of MDLastResortStatusSpec.
func (o MDLastResortStatusSpec) DeepCopy() MDLastResortStatusSpec {
	cp := o

	if o.Pending != nil {
		cp.Pending = make([]string, len(o.Pending))
		copy(cp.Pending, o.Pending)
	}

	return cp
}
