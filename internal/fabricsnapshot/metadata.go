package fabricsnapshot

type Metadata struct {
	ChannelName            string            `json:"channel_name"`
	LastBlockNumber        uint64            `json:"last_block_number"`
	LastBlockHash          string            `json:"last_block_hash"`
	PreviousBlockHash      string            `json:"previous_block_hash"`
	SnapshotFilesRawHashes map[string]string `json:"snapshot_files_raw_hashes"`
	StateDBType            string            `json:"state_db_type"`
}
