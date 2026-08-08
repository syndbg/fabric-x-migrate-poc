package genesisdata

type Input struct {
	ExporterVersion   string
	Source            Source
	NamespaceMappings []NamespaceMapping
	PublicState       []*StateRecord
	TransactionIDs    []*TransactionIDRecord
	Exclusions        []Exclusion
}
