package commitmsg

type Mode string

const (
	ModeNormal Mode = "normal"
	ModeAmend  Mode = "amend"
)

type Request struct {
	Mode Mode
}
