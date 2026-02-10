package version

// Static build metadata. Update Version when making a release.
const (
	Version   = "0.1.0"
	Commit    = ""
	BuildTime = ""
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
}