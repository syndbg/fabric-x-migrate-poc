package fabricsnapshot

type Record struct {
	Namespace         string
	Key               []byte
	Value             []byte
	Metadata          []byte
	Version           []byte
	BlockNumber       uint64
	TransactionNumber uint64
}
