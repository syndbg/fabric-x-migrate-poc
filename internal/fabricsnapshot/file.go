package fabricsnapshot

type File struct {
	Name       string
	Size       int64
	SHA256     string
	FormatByte *uint8
}
