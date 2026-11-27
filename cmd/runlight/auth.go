package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/actions/scaleset"
)

// newClient builds the GitHub client. Credentials come from the environment,
// never from flags, so they cannot leak through `ps` or shell history.
//
//	GitHub App (recommended): RUNLIGHT_APP_CLIENT_ID, RUNLIGHT_APP_INSTALLATION_ID,
//	                          RUNLIGHT_APP_PRIVATE_KEY_FILE
//	Personal access token:    RUNLIGHT_TOKEN
func newClient(url string) (*scaleset.Client, error) {
	if id := os.Getenv("RUNLIGHT_APP_CLIENT_ID"); id != "" {
		inst, err := strconv.ParseInt(os.Getenv("RUNLIGHT_APP_INSTALLATION_ID"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("RUNLIGHT_APP_INSTALLATION_ID: %w", err)
		}
		key, err := os.ReadFile(os.Getenv("RUNLIGHT_APP_PRIVATE_KEY_FILE"))
		if err != nil {
			return nil, fmt.Errorf("RUNLIGHT_APP_PRIVATE_KEY_FILE: %w", err)
		}
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: url,
			GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: id, InstallationID: inst, PrivateKey: string(key)},
			SystemInfo:      sysInfo(0),
		})
	}
	if tok := os.Getenv("RUNLIGHT_TOKEN"); tok != "" {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     url,
			PersonalAccessToken: tok,
			SystemInfo:          sysInfo(0),
		})
	}
	return nil, errors.New("no credentials: set RUNLIGHT_TOKEN or the RUNLIGHT_APP_* variables")
}
