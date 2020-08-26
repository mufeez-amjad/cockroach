// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package doctor_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/doctor"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/stretchr/testify/require"
)

func TestExamine(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	toBytes := func(desc *descpb.Descriptor) []byte {
		res, err := protoutil.Marshal(desc)
		require.NoError(t, err)
		return res
	}

	tests := []struct {
		descTable      doctor.DescriptorTable
		namespaceTable doctor.NamespaceTable
		valid          bool
		errStr         string
		expected       string
	}{
		{
			valid:    true,
			expected: "Examining 0 descriptors and 0 namespace entries...\n",
		},
		{
			descTable: doctor.DescriptorTable{{ID: 1, DescBytes: []byte("#$@#@#$#@#")}},
			errStr:    "failed to unmarshal descriptor",
			expected:  "Examining 1 descriptors and 0 namespace entries...\n",
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Table{
						Table: &descpb.TableDescriptor{ID: 2},
					}}),
				},
			},
			expected: `Examining 1 descriptors and 0 namespace entries...
   Table   2: ParentID   0, ParentSchemaID 29, Name '': different id in descriptor table: 1
`,
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Table{
						Table: &descpb.TableDescriptor{Name: "foo", ID: 1, State: descpb.DescriptorState_DROP},
					}}),
				},
			},
			expected: `Examining 1 descriptors and 0 namespace entries...
   Table   1: ParentID   0, ParentSchemaID 29, Name 'foo': invalid parent ID 0
`,
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Table{
						Table: &descpb.TableDescriptor{Name: "foo", ID: 1},
					}}),
				},
			},
			namespaceTable: doctor.NamespaceTable{
				{NameInfo: descpb.NameInfo{ParentSchemaID: 29, Name: "foo"}, ID: 1},
			},
			expected: `Examining 1 descriptors and 1 namespace entries...
   Table   1: ParentID   0, ParentSchemaID 29, Name 'foo': invalid parent ID 0
`,
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Database{
						Database: &descpb.DatabaseDescriptor{Name: "db", ID: 1},
					}}),
				},
			},
			expected: `Examining 1 descriptors and 0 namespace entries...
Database   1: ParentID   0, ParentSchemaID  0, Name 'db': not being dropped but no namespace entry found
`,
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Table{
						Table: &descpb.TableDescriptor{
							Name: "t", ID: 1, ParentID: 2,
							Columns: []descpb.ColumnDescriptor{
								{Name: "col", ID: 1, Type: types.Int},
							},
							NextColumnID: 2,
							Families: []descpb.ColumnFamilyDescriptor{
								{ID: 0, Name: "f", ColumnNames: []string{"col"}, ColumnIDs: []descpb.ColumnID{1}, DefaultColumnID: 1},
							},
							NextFamilyID: 1,
							PrimaryIndex: descpb.IndexDescriptor{
								Name:             tabledesc.PrimaryKeyIndexName,
								ID:               1,
								Unique:           true,
								ColumnNames:      []string{"col"},
								ColumnDirections: []descpb.IndexDescriptor_Direction{descpb.IndexDescriptor_ASC},
								ColumnIDs:        []descpb.ColumnID{1},
								Version:          descpb.SecondaryIndexFamilyFormatVersion,
							},
							NextIndexID: 2,
							Privileges: descpb.NewCustomSuperuserPrivilegeDescriptor(
								descpb.SystemAllowedPrivileges[keys.SqllivenessID], security.NodeUser),
							FormatVersion:  descpb.InterleavedFormatVersion,
							NextMutationID: 1,
						},
					}}),
				},
				{
					ID: 2,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Database{
						Database: &descpb.DatabaseDescriptor{Name: "db", ID: 2},
					}}),
				},
			},
			namespaceTable: doctor.NamespaceTable{
				{NameInfo: descpb.NameInfo{ParentSchemaID: 29, Name: "t"}, ID: 1},
				{NameInfo: descpb.NameInfo{Name: "db"}, ID: 2},
			},
			expected: `Examining 2 descriptors and 2 namespace entries...
   Table   1: ParentID   2, ParentSchemaID 29, Name 't': could not find name in namespace table
`,
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Schema{
						Schema: &descpb.SchemaDescriptor{Name: "schema", ID: 1, ParentID: 2},
					}}),
				},
			},
			namespaceTable: doctor.NamespaceTable{
				{NameInfo: descpb.NameInfo{ParentID: 2, Name: "schema"}, ID: 1},
			},
			expected: `Examining 1 descriptors and 1 namespace entries...
  Schema   1: ParentID   2, ParentSchemaID  0, Name 'schema': invalid parent id 2
`,
		},
		{
			descTable: doctor.DescriptorTable{
				{
					ID: 1,
					DescBytes: toBytes(&descpb.Descriptor{Union: &descpb.Descriptor_Type{
						Type: &descpb.TypeDescriptor{Name: "type", ID: 1},
					}}),
				},
			},
			namespaceTable: doctor.NamespaceTable{
				{NameInfo: descpb.NameInfo{Name: "type"}, ID: 1},
			},
			expected: `Examining 1 descriptors and 1 namespace entries...
    Type   1: ParentID   0, ParentSchemaID  0, Name 'type': invalid parentID 0
`,
		},
		{
			namespaceTable: doctor.NamespaceTable{
				{NameInfo: descpb.NameInfo{Name: "foo"}, ID: keys.PublicSchemaID},
				{NameInfo: descpb.NameInfo{Name: "bar"}, ID: keys.PublicSchemaID},
				{NameInfo: descpb.NameInfo{Name: "pg_temp_foo"}, ID: 1},
				{NameInfo: descpb.NameInfo{Name: "causes_error"}, ID: 2},
			},
			expected: `Examining 0 descriptors and 4 namespace entries...
Descriptor 2: has namespace row(s) [{ParentID:0 ParentSchemaID:0 Name:causes_error}] but no descriptor
`,
		},
		{
			namespaceTable: doctor.NamespaceTable{
				{NameInfo: descpb.NameInfo{Name: "null"}, ID: int64(descpb.InvalidID)},
			},
			expected: `Examining 0 descriptors and 1 namespace entries...
Row(s) [{ParentID:0 ParentSchemaID:0 Name:null}]: NULL value found
`,
		},
	}

	for i, test := range tests {
		var buf bytes.Buffer
		valid, err := doctor.Examine(
			context.Background(), test.descTable, test.namespaceTable, false, &buf)
		msg := fmt.Sprintf("Test %d failed!", i+1)
		if test.errStr != "" {
			require.Containsf(t, err.Error(), test.errStr, msg)
		} else {
			require.NoErrorf(t, err, msg)
		}
		require.Equalf(t, test.valid, valid, msg)
		require.Equalf(t, test.expected, buf.String(), msg)
	}
}
