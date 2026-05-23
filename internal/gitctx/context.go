package gitctx

type Repository struct {
	RootPath   string
	WorkPath   string
	Branch     string
	HeadSHA    string
	IsDetached bool
}
