package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/syndbg/fabric-x-migrate-poc/internal/fabricsnapshot"
	"github.com/syndbg/fabric-x-migrate-poc/pkg/genesisdata"
)

func TestExportInput(t *testing.T) {
	t.Run("when multiple namespaces are mapped, it exports each selected namespace", func(t *testing.T) {
		snapshot := &fabricsnapshot.Snapshot{
			Metadata: fabricsnapshot.Metadata{ChannelName: "channel-a", StateDBType: "SimpleKeyValueDB"},
			PublicNamespaces: []fabricsnapshot.Namespace{
				{Name: "assets", Records: 1},
				{Name: "payments", Records: 1},
				{Name: "_lifecycle", Records: 1},
			},
			PrivateHashNamespaces: []fabricsnapshot.Namespace{{Name: "assets$$h_private", Records: 2}},
			ConfigHistoryRecords:  1,
			PublicRecords: []fabricsnapshot.Record{
				{Namespace: "assets", Key: []byte("asset-a"), Value: []byte("value-a")},
				{Namespace: "payments", Key: []byte("payment-a"), Value: []byte("value-b")},
				{Namespace: "_lifecycle", Key: []byte("definition"), Value: []byte("system")},
			},
			TransactionIDs: []string{"tx-a"},
		}
		mappings := []genesisdata.NamespaceMapping{
			{Source: "assets", Target: "channel_a_assets"},
			{Source: "payments", Target: "channel_a_payments"},
		}

		input, err := exportInput(snapshot, "3.1.5", mappings)
		require.NoError(t, err)
		require.Len(t, input.PublicState, 2)
		assert.Equal(t, "channel_a_assets", input.PublicState[0].TargetNamespace)
		assert.Equal(t, "channel_a_payments", input.PublicState[1].TargetNamespace)
		require.Len(t, input.Exclusions, 3)
		assert.ElementsMatch(t, []string{"collection_config_history", "private_data_hashes", "public_namespace"}, []string{
			input.Exclusions[0].Kind, input.Exclusions[1].Kind, input.Exclusions[2].Kind,
		})
		require.Len(t, input.TransactionIDs, 1)
		assert.Equal(t, "tx-a", input.TransactionIDs[0].TransactionId)
	})

	t.Run("when a selected source namespace is absent, it rejects the export", func(t *testing.T) {
		snapshot := &fabricsnapshot.Snapshot{
			Metadata:      fabricsnapshot.Metadata{ChannelName: "channel-a", StateDBType: "SimpleKeyValueDB"},
			PublicRecords: []fabricsnapshot.Record{{Namespace: "assets", Key: []byte("asset-a"), Value: []byte("value-a")}},
		}

		_, err := exportInput(snapshot, "3.1.5", []genesisdata.NamespaceMapping{{Source: "payments", Target: "payments"}})
		require.ErrorContains(t, err, `source namespace "payments" has no public records`)
	})

	t.Run("when a selected source namespace is empty, it rejects the export", func(t *testing.T) {
		snapshot := &fabricsnapshot.Snapshot{
			Metadata:         fabricsnapshot.Metadata{ChannelName: "channel-a", StateDBType: "SimpleKeyValueDB"},
			PublicNamespaces: []fabricsnapshot.Namespace{{Name: "payments", Records: 0}},
		}

		_, err := exportInput(snapshot, "3.1.5", []genesisdata.NamespaceMapping{{Source: "payments", Target: "payments"}})
		require.ErrorContains(t, err, `source namespace "payments" has no public records`)
	})

	t.Run("when a selected key has metadata, it rejects the export", func(t *testing.T) {
		snapshot := &fabricsnapshot.Snapshot{
			Metadata:         fabricsnapshot.Metadata{ChannelName: "channel-a", StateDBType: "SimpleKeyValueDB"},
			PublicNamespaces: []fabricsnapshot.Namespace{{Name: "assets", Records: 1}},
			PublicRecords: []fabricsnapshot.Record{{
				Namespace: "assets", Key: []byte("asset-a"), Value: []byte("value-a"), Metadata: []byte("state-based-endorsement"),
			}},
		}

		_, err := exportInput(snapshot, "3.1.5", []genesisdata.NamespaceMapping{{Source: "assets", Target: "assets"}})
		require.ErrorContains(t, err, `namespace "assets" contains key metadata`)
	})
}

func TestNamespaceMappingsSet(t *testing.T) {
	t.Run("when the flag repeats, it retains every mapping", func(t *testing.T) {
		var mappings namespaceMappings
		require.NoError(t, mappings.Set("assets=channel_a_assets"))
		require.NoError(t, mappings.Set("payments=channel_a_payments"))
		assert.Len(t, mappings, 2)
		assert.Equal(t, "assets=channel_a_assets,payments=channel_a_payments", mappings.String())
	})

	t.Run("when the mapping has no separator, it rejects the value", func(t *testing.T) {
		var mappings namespaceMappings
		require.Error(t, mappings.Set("invalid"))
	})
}
