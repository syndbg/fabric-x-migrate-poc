package fabricsnapshot

type AdditionalMetadata struct {
	SnapshotHash        string `json:"snapshot_hash"`
	LastBlockCommitHash string `json:"last_block_commit_hash"`
}
