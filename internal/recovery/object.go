package recovery

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type recoveryObject struct {
	url   string
	netrc string
}

func newRecoveryObject(endpoint, netrc, path string) (recoveryObject, error) {
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil ||
		(target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.Fragment != "" {
		return recoveryObject{}, errors.New("recovery object storage requires an HTTPS endpoint")
	}
	credential, err := os.Stat(netrc)
	if err != nil || !credential.Mode().IsRegular() || credential.Mode().Perm()&0o077 != 0 {
		return recoveryObject{}, errors.New("recovery object storage requires a private netrc credential file")
	}
	target.Path = path
	return recoveryObject{url: target.String(), netrc: netrc}, nil
}

func (object recoveryObject) request(ctx context.Context, args ...string) (string, error) {
	flags := []string{
		"-q", "--silent", "--show-error", "--proto", "=https", "--max-time", "20",
		"--netrc-file", object.netrc, "--aws-sigv4", "aws:amz:auto:s3", "--write-out", "%{http_code}",
	}
	// #nosec G204 -- Fixed HTTPS endpoint and no credentials on the command line.
	out, err := exec.CommandContext(ctx, "curl", append(append(flags, args...), object.url)...).Output()
	if err != nil {
		return "", fmt.Errorf("send recovery object request: %w", err)
	}
	return string(out), nil
}

func (object recoveryObject) writeCondition(ctx context.Context, headers string) (string, error) {
	status, err := object.request(ctx, "--head", "--dump-header", headers, "--output", os.DevNull)
	if err != nil {
		return "", err
	}
	switch status {
	case "200":
		return objectMatchHeader(headers)
	case "404":
		return "If-None-Match: *", nil
	default:
		return "", fmt.Errorf("object store refused lookup (HTTP %s)", status)
	}
}

func objectMatchHeader(headers string) (string, error) {
	data, err := os.ReadFile(headers) // #nosec G304 -- This is curl output in the private scratch directory.
	if err != nil {
		return "", fmt.Errorf("read recovery object ETag: %w", err)
	}
	etag := ""
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, found := strings.Cut(line, ":"); found && strings.EqualFold(name, "etag") {
			etag = strings.TrimSpace(value)
		}
	}
	if _, err := strconv.Unquote(etag); err != nil || !strings.HasPrefix(etag, "\"") {
		return "", errors.New("object store returned no usable recovery object ETag")
	}
	return "If-Match: " + etag, nil
}
