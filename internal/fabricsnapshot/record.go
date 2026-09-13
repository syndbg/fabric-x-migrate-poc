package fabricsnapshot

type Record struct {
	Namespace string
	Key       []byte
	Value     []byte
	Metadata  []byte
}
