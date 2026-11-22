package devicefarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm"
	"github.com/aws/aws-sdk-go-v2/service/devicefarm/types"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

// Upload limits and timing; tests shorten them.
var (
	maxUploadSize      int64 = 4 << 30
	uploadReuseAge           = 30 * 24 * time.Hour
	uploadPollInterval       = 2 * time.Second
	uploadPollTimeout        = 300 * time.Second
)

// uploadType maps an app file's extension to its upload type.
func uploadType(path string) (types.UploadType, string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".apk":
		return types.UploadTypeAndroidApp, ext, nil
	case ".ipa":
		return types.UploadTypeIosApp, ext, nil
	}
	return "", "", cloudprov.Errorf(cloudprov.Other,
		"Device Farm accepts only .apk and .ipa apps, not %q (an .aab must be built as an .apk)", filepath.Base(path))
}

// IsUploadARN reports whether ref is a Device Farm upload ARN rather than a
// local path.
func IsUploadARN(ref string) bool {
	return strings.HasPrefix(ref, "arn:aws:devicefarm:") && strings.Contains(ref, ":upload:")
}

// ResolveApp returns the upload ARN for ref: an upload ARN passes through, a
// local path is uploaded (or an identical earlier upload reused).
func (c *Client) ResolveApp(ctx context.Context, ref string) (string, error) {
	if IsUploadARN(ref) {
		return ref, nil
	}
	app, err := c.Upload(ctx, ref)
	if err != nil {
		return "", err
	}
	return app.Ref, nil
}

// Upload uploads the .apk or .ipa at path to the project. The upload is
// named after the file's content, so a SUCCEEDED upload of the same content
// younger than 30 days is reused instead.
func (c *Client) Upload(ctx context.Context, path string) (*cloudprov.AppRef, error) {
	typ, ext, err := uploadType(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, cloudprov.Errorf(cloudprov.Other, "app %s: %v", filepath.Base(path), err)
	}
	if !st.Mode().IsRegular() {
		return nil, cloudprov.Errorf(cloudprov.Other, "app %s is not a regular file", filepath.Base(path))
	}
	if st.Size() > maxUploadSize {
		return nil, cloudprov.Errorf(cloudprov.Other, "app %s is %d bytes; Device Farm accepts at most %d",
			filepath.Base(path), st.Size(), maxUploadSize)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return nil, cloudprov.Errorf(cloudprov.Other, "app %s: %v", filepath.Base(path), err)
	}
	name := "maestro-d-" + sum[:20] + ext
	project, err := c.Project(ctx)
	if err != nil {
		return nil, err
	}
	uploads, err := c.listUploads(ctx, project, typ)
	if err != nil {
		return nil, err
	}
	for _, u := range uploads {
		if str(u.Name) == name && u.Status == types.UploadStatusSucceeded &&
			u.Created != nil && c.now().Sub(*u.Created) < uploadReuseAge {
			ref := appRef(u)
			ref.Reused = true
			return &ref, nil
		}
	}
	out, err := c.api.CreateUpload(ctx, &devicefarm.CreateUploadInput{
		ProjectArn:  aws.String(project),
		Name:        aws.String(name),
		Type:        typ,
		ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return nil, err
	}
	if out.Upload == nil || str(out.Upload.Url) == "" || str(out.Upload.Arn) == "" {
		return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm CreateUpload returned no upload URL")
	}
	if err := c.put(ctx, str(out.Upload.Url), path, st.Size()); err != nil {
		return nil, err
	}
	u, err := c.waitUpload(ctx, str(out.Upload.Arn))
	if err != nil {
		return nil, err
	}
	ref := appRef(*u)
	return &ref, nil
}

// put sends the file to the presigned upload URL.
func (c *Client) put(ctx context.Context, url, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return cloudprov.Errorf(cloudprov.Other, "app %s: %v", filepath.Base(path), err)
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return cloudprov.Errorf(cloudprov.Other, "Device Farm upload: %s", Redact(err.Error()))
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return cloudprov.Errorf(cloudprov.Other, "Device Farm upload: %s", Redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return cloudprov.Errorf(cloudprov.Other, "Device Farm upload: HTTP %d: %s",
			resp.StatusCode, Redact(strings.TrimSpace(string(body))))
	}
	return nil
}

// waitUpload polls an upload until it SUCCEEDED or FAILED.
func (c *Client) waitUpload(ctx context.Context, arn string) (*types.Upload, error) {
	deadline := time.Now().Add(uploadPollTimeout)
	for {
		out, err := c.api.GetUpload(ctx, &devicefarm.GetUploadInput{Arn: aws.String(arn)})
		if err != nil {
			return nil, err
		}
		u := out.Upload
		if u == nil {
			return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm GetUpload returned no upload")
		}
		switch u.Status {
		case types.UploadStatusSucceeded:
			return u, nil
		case types.UploadStatusFailed:
			msg := str(u.Message)
			if meta := str(u.Metadata); meta != "" {
				msg += " (" + meta + ")"
			}
			return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm rejected the upload %s: %s", str(u.Name), Redact(msg))
		}
		if !time.Now().Before(deadline) {
			return nil, cloudprov.Errorf(cloudprov.Other, "Device Farm did not process the upload %s within %s (status %s)",
				str(u.Name), uploadPollTimeout, u.Status)
		}
		if err := sleep(ctx, uploadPollInterval); err != nil {
			return nil, err
		}
	}
}

// Apps lists the project's app uploads (ANDROID_APP and IOS_APP).
func (c *Client) Apps(ctx context.Context) ([]cloudprov.AppRef, error) {
	project, err := c.Project(ctx)
	if err != nil {
		return nil, err
	}
	uploads, err := c.listUploads(ctx, project, "")
	if err != nil {
		return nil, err
	}
	apps := []cloudprov.AppRef{}
	for _, u := range uploads {
		if u.Type == types.UploadTypeAndroidApp || u.Type == types.UploadTypeIosApp {
			apps = append(apps, appRef(u))
		}
	}
	return apps, nil
}

// listUploads pages ListUploads for the project, of one type or all.
func (c *Client) listUploads(ctx context.Context, project string, typ types.UploadType) ([]types.Upload, error) {
	var all []types.Upload
	pages := devicefarm.NewListUploadsPaginator(c.api, &devicefarm.ListUploadsInput{Arn: aws.String(project), Type: typ})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Uploads...)
	}
	return all, nil
}

// appRef describes an upload without its URL.
func appRef(u types.Upload) cloudprov.AppRef {
	ref := cloudprov.AppRef{
		Ref:    str(u.Arn),
		Name:   str(u.Name),
		Type:   string(u.Type),
		Status: string(u.Status),
	}
	if u.Created != nil {
		ref.Created = u.Created.UTC().Format(time.RFC3339)
	}
	switch u.Type {
	case types.UploadTypeAndroidApp:
		ref.Platform = "android"
	case types.UploadTypeIosApp:
		ref.Platform = "ios"
	}
	return ref
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
