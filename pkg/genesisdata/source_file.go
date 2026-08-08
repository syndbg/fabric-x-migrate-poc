package genesisdata

type SourceFile struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	FormatByte *uint8 `json:"format_byte,omitempty"`
}
