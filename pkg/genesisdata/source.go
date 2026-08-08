package genesisdata

type Source struct {
	FabricVersion       string       `json:"fabric_version"`
	Channel             string       `json:"channel"`
	LastBlockNumber     uint64       `json:"last_block_number"`
	LastBlockHash       string       `json:"last_block_hash"`
	PreviousBlockHash   string       `json:"previous_block_hash"`
	LastBlockCommitHash string       `json:"last_block_commit_hash,omitempty"`
	SnapshotHash        string       `json:"snapshot_hash,omitempty"`
	StateDBType         string       `json:"state_db_type"`
	Files               []SourceFile `json:"files"`
}
