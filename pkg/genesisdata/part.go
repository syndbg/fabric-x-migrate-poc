package genesisdata

type Part struct {
	Name          string `json:"name"`
	Message       string `json:"message"`
	FormatVersion uint8  `json:"format_version"`
	RecordCount   uint64 `json:"record_count"`
	SHA256        string `json:"sha256"`
}
