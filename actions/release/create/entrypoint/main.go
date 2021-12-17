package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

type Release struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Name            string `json:"name"`
	Body            string `json:"body,omitempty"`
	Draft           bool   `json:"draft"`
}

func main() {
	var config struct {
		Endpoint   string
		Repo       string
		Token      string
		Release    Release
		Draft      bool
		Assets     string
		MaxRetries int
	}

	flag.StringVar(&config.Endpoint, "endpoint", "https://api.github.com", "Specifies endpoint for sending requests")
	flag.StringVar(&config.Repo, "repo", "", "Specifies repo for sending requests")
	flag.StringVar(&config.Token, "token", "", "Github Authorization Token")
	flag.StringVar(&config.Release.TagName, "tag-name", "", "Name of the tag for the release")
	flag.StringVar(&config.Release.TargetCommitish, "target-commitish", "", "Commitish that is being tagged and released")
	flag.StringVar(&config.Release.Name, "name", "", "Name of release")
	flag.StringVar(&config.Release.Body, "body", "", "Contents of release body")
	flag.BoolVar(&config.Draft, "draft", false, "Sets the release as a draft")
	flag.StringVar(&config.Assets, "assets", "", "JSON-encoded assets metadata")
	flag.IntVar(&config.MaxRetries, "max-retries", 0, "Max retries to upload each asset")
	flag.Parse()

	if config.Repo == "" {
		fail(errors.New(`missing required input "repo"`))
	}

	if config.Token == "" {
		fail(errors.New(`missing required input "token"`))
	}

	if config.Release.TagName == "" {
		fail(errors.New(`missing required input "tag_name"`))
	}

	if config.Release.TargetCommitish == "" {
		fail(errors.New(`missing required input "target_commitish"`))
	}

	if config.Release.Name == "" {
		fail(errors.New(`missing required input "name"`))
	}

	var assets []struct {
		Path        string `json:"path"`
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
	}

	if config.Assets != "" {
		err := json.Unmarshal([]byte(config.Assets), &assets)
		if err != nil {
			fail(fmt.Errorf("failed to parse assets: %w", err))
		}
	}

	config.Release.Draft = true
	body := bytes.NewBuffer(nil)
	err := json.NewEncoder(body).Encode(config.Release)
	if err != nil {
		fail(fmt.Errorf("failed to encode release: %w", err))
	}

	fmt.Println("Creating release")
	fmt.Printf("  Repository: %s\n", config.Repo)
	uri := fmt.Sprintf("%s/repos/%s/releases", config.Endpoint, config.Repo)
	req, err := http.NewRequest("POST", uri, body)
	if err != nil {
		fail(fmt.Errorf("failed to create request: %w", err))
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", config.Token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(fmt.Errorf("failed to complete request: %w", err))
	}

	if resp.StatusCode != http.StatusCreated {
		dump, _ := httputil.DumpResponse(resp, true)
		fail(fmt.Errorf("failed to create release: unexpected response: %s", dump))
	}

	var release struct {
		ID        int    `json:"id"`
		UploadURL string `json:"upload_url"`
	}

	err = json.NewDecoder(resp.Body).Decode(&release)
	if err != nil {
		fail(fmt.Errorf("failed to parse create release response: %w", err))
	}

	retries := 0
	for _, asset := range assets {
		for {
			err := uploadAsset(release.ID, release.UploadURL, asset.Name, asset.Path, asset.ContentType, config.Repo, config.Token)
			if err == nil {
				break
			} else if retries == config.MaxRetries {
				fmt.Printf("Failed uploading asset after %d retries\n", retries)
				fail(err)
			}
			retries++
			fmt.Printf("Failed uploading asset. Retrying (retry #%d)\n", retries)
			time.Sleep(time.Second * 60)
		}
	}

	if config.Draft {
		fmt.Println("Release is drafted, exiting.")
		return
	}

	uri = fmt.Sprintf("%s/repos/%s/releases/%d", config.Endpoint, config.Repo, release.ID)
	req, err = http.NewRequest("PATCH", uri, strings.NewReader(`{"draft": false}`))
	if err != nil {
		fail(fmt.Errorf("failed to create request: %w", err))
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", config.Token))

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail(fmt.Errorf("failed to complete request: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		dump, _ := httputil.DumpResponse(resp, true)
		fail(fmt.Errorf("failed to edit release: unexpected response: %s", dump))
	}

	fmt.Println("Release is published, exiting.")
}

// UploadAsset will upload the given asset to the given release of the given
// repo using the given token.
func uploadAsset(releaseId int, releaseUploadURL, assetName, assetPath, assetContentType, repo, token string) error {
	uri, err := url.Parse(releaseUploadURL)
	if err != nil {
		return fmt.Errorf("failed to parse upload url: %w", err)
	}

	uri.Path = fmt.Sprintf("/repos/%s/releases/%d/assets", repo, releaseId)
	uri.RawQuery = url.Values{"name": []string{assetName}}.Encode()

	file, err := os.Open(assetPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	req, err := http.NewRequest("POST", uri.String(), file)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", assetContentType)
	req.GetBody = func() (io.ReadCloser, error) {
		return os.Open(assetPath)
	}
	// prevents re-use of TCP connections between requests
	req.Close = true

	fmt.Printf("  Uploading asset: %s -> %s\n", assetPath, assetName)
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 20,
		},
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to complete request: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		dump, _ := httputil.DumpResponse(resp, true)
		return fmt.Errorf("failed to upload asset: unexpected response: %s", dump)
	}
	return nil
}

func fail(err error) {
	fmt.Printf("Error: %s", err)
	os.Exit(1)
}
