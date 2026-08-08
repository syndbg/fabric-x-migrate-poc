package genesisdata

type Exclusion struct {
	Kind        string   `json:"kind"`
	Subject     string   `json:"subject"`
	RecordCount uint64   `json:"record_count,omitempty"`
	SourceFiles []string `json:"source_files,omitempty"`
	Reason      string   `json:"reason"`
}
