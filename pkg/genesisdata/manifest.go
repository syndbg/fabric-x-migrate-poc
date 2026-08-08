package genesisdata

type Manifest struct {
	Format            string             `json:"format"`
	FormatVersion     uint32             `json:"format_version"`
	ExporterVersion   string             `json:"exporter_version"`
	HashAlgorithm     string             `json:"hash_algorithm"`
	Source            Source             `json:"source"`
	NamespaceMappings []NamespaceMapping `json:"namespace_mappings"`
	Parts             []Part             `json:"parts"`
	Exclusions        []Exclusion        `json:"exclusions"`
}
