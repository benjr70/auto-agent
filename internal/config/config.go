// Package config reads what the Daemon is told rather than what it derives:
// the Target Project's Harness config and the machine's Host config. Both
// come back as one validated value, or as an error naming the key at fault.
package config

// Config is everything the Daemon is configured with for one Target Project.
type Config struct {
	Harness Harness
	Host    Host
}

// Input is what Load reads from.
type Input struct {
	// TargetDir is the Target Project checkout holding .auto-agent.
	TargetDir string
	// HarnessFile overrides the harness.json to read; empty means TargetDir's.
	HarnessFile string
	// Repo is the Target Project on GitHub, which no file declares.
	Repo Repo
	// LookupEnv reads the environment, as os.LookupEnv does.
	LookupEnv func(string) (string, bool)
}

// Load reads and validates the Harness config and the Host config.
func Load(in Input) (Config, error) {
	harness, err := LoadHarness(in.TargetDir, in.HarnessFile, in.Repo)
	if err != nil {
		return Config{}, err
	}
	host, err := LoadHost(in.Repo, in.LookupEnv)
	if err != nil {
		return Config{}, err
	}
	return Config{Harness: harness, Host: host}, nil
}
