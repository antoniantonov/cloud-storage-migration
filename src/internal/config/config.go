package config

type Mode string

const (
	List Mode = "list"
	Copy Mode = "copy"
)

type Config struct {
	Mode                  Mode
	ItemIDs               []string
	MicrosoftClientID     string
	GoogleCredentialsFile string
	AuthDir               string
	TempCopyDir           string
	InteractiveAuth       bool
	GoogleOAuthPort       int
	GoogleOAuthListenHost string
}
