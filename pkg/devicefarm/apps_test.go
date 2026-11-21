package devicefarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
)

func appName(content, ext string) string {
	sum := sha256.Sum256([]byte(content))
	return "maestro-d-" + hex.EncodeToString(sum[:])[:20] + ext
}

func TestUploadRejectsFormatsAndSize(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	for _, name := range []string{"app.aab", "app.zip", "app"} {
		_, err := c.Upload(ctx, writeFile(t, name, "x"))
		if cloudprov.KindOf(err) != cloudprov.Other || !strings.Contains(err.Error(), ".apk and .ipa") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := c.Upload(ctx, filepath.Join(t.TempDir(), "missing.apk")); err == nil {
		t.Error("a missing file uploaded")
	}
	dir := filepath.Join(t.TempDir(), "dir.apk")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(ctx, dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("dir: err = %v", err)
	}
	saved := maxUploadSize
	maxUploadSize = 4
	defer func() { maxUploadSize = saved }()
	if _, err := c.Upload(ctx, writeFile(t, "big.apk", "12345")); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Errorf("big: err = %v", err)
	}
	if len(f.Requests()) != 0 {
		t.Fatalf("rejected apps made requests: %v", f.Ops())
	}
}

func TestUploadNewThenReused(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	path := writeFile(t, "App.APK", "android app")
	ref, err := c.Upload(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	want := appName("android app", ".apk")
	if ref.Name != want || ref.Reused || ref.Status != "SUCCEEDED" || ref.Platform != "android" || ref.Type != "ANDROID_APP" || ref.Created == "" {
		t.Fatalf("ref = %+v", ref)
	}
	body := lastBody(t, f, "CreateUpload")
	if body["name"] != want || body["type"] != "ANDROID_APP" || body["contentType"] != "application/octet-stream" {
		t.Fatalf("CreateUpload = %v", body)
	}
	ups := f.Uploads()
	if len(ups) != 1 || ups[0].PutContentType != "application/octet-stream" || ups[0].Size != int64(len("android app")) {
		t.Fatalf("uploads = %+v", ups)
	}
	again, err := c.Upload(ctx, path)
	if err != nil || !again.Reused || again.Ref != ref.Ref {
		t.Fatalf("again = %+v, %v", again, err)
	}
	if f.CountOp("CreateUpload") != 1 {
		t.Fatalf("ops = %v", f.Ops())
	}
	// An .ipa is an iOS app.
	ipa, err := c.Upload(ctx, writeFile(t, "a.ipa", "ios app"))
	if err != nil || ipa.Platform != "ios" || ipa.Type != "IOS_APP" {
		t.Fatalf("ipa = %+v, %v", ipa, err)
	}
}

func TestUploadStaleOrFailedIsUploadedAgain(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	project, _ := c.Project(ctx)
	name := appName("same", ".apk")
	f.SeedUpload(project, name, "ANDROID_APP", "SUCCEEDED", time.Now().Add(-31*24*time.Hour))
	f.SeedUpload(project, name, "ANDROID_APP", "FAILED", time.Now())
	ref, err := c.Upload(ctx, writeFile(t, "a.apk", "same"))
	if err != nil || ref.Reused {
		t.Fatalf("ref = %+v, %v", ref, err)
	}
	if f.CountOp("CreateUpload") != 1 {
		t.Fatalf("ops = %v", f.Ops())
	}
}

func TestUploadFailedProcessing(t *testing.T) {
	c, f := newTestClient(t)
	f.FailUploads("The app is not signed", `{"errorCode":"INVALID_APP"}`)
	_, err := c.Upload(context.Background(), writeFile(t, "a.apk", "bad"))
	if err == nil || !strings.Contains(err.Error(), "The app is not signed") || !strings.Contains(err.Error(), "INVALID_APP") {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadPollTimeoutAndCancel(t *testing.T) {
	c, f := newTestClient(t)
	f.SetUploadDelay(time.Hour)
	uploadPollTimeout = 30 * time.Millisecond
	_, err := c.Upload(context.Background(), writeFile(t, "a.apk", "slow"))
	if err == nil || !strings.Contains(err.Error(), "did not process") || !strings.Contains(err.Error(), "PROCESSING") {
		t.Fatalf("err = %v", err)
	}
	uploadPollTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Upload(ctx, writeFile(t, "b.apk", "slower")); err == nil {
		t.Fatal("a cancelled poll succeeded")
	}
}

func TestUploadAPIErrors(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	path := writeFile(t, "a.apk", "x")
	for _, op := range []string{"ListProjects", "ListUploads", "CreateUpload", "GetUpload"} {
		f.FailOp(op, "ArgumentException", "nope")
		if _, err := c.Upload(ctx, path); err == nil {
			t.Errorf("%s failure ignored", op)
		}
		f.FailOp(op, "", "")
	}
}

func TestPutFailuresAreRedacted(t *testing.T) {
	c, _ := newTestClient(t)
	path := writeFile(t, "a.apk", "x")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied for https://bucket.s3.us-west-2.amazonaws.com/k?X-Amz-Signature=deadbeef", http.StatusForbidden)
	}))
	err := c.put(context.Background(), srv.URL+"/k?X-Amz-Signature=deadbeef", path, 1)
	if err == nil || strings.Contains(err.Error(), "deadbeef") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v", err)
	}
	srv.Close()
	err = c.put(context.Background(), srv.URL+"/k?X-Amz-Signature=deadbeef", path, 1)
	if err == nil || strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("closed server: err = %v", err)
	}
	if err := c.put(context.Background(), "http://bad host/", path, 1); err == nil {
		t.Fatal("bad URL accepted")
	}
	if err := c.put(context.Background(), srv.URL, filepath.Join(t.TempDir(), "gone.apk"), 1); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestResolveAppAndApps(t *testing.T) {
	c, f := newTestClient(t)
	ctx := context.Background()
	arn := "arn:aws:devicefarm:us-west-2:123456789012:upload:p/u"
	if got, err := c.ResolveApp(ctx, arn); err != nil || got != arn || len(f.Requests()) != 0 {
		t.Fatalf("ARN: %q, %v, %v", got, err, f.Ops())
	}
	got, err := c.ResolveApp(ctx, writeFile(t, "a.apk", "x"))
	if err != nil || !IsUploadARN(got) {
		t.Fatalf("path: %q, %v", got, err)
	}
	if _, err := c.ResolveApp(ctx, "a.txt"); err == nil {
		t.Fatal("a .txt resolved")
	}
	project, _ := c.Project(ctx)
	f.SeedUpload(project, "tests.zip", "APPIUM_NODE_TEST_PACKAGE", "SUCCEEDED", time.Now())
	f.SeedUpload(project, "b.ipa", "IOS_APP", "SUCCEEDED", time.Now())
	apps, err := c.Apps(ctx)
	if err != nil || len(apps) != 2 {
		t.Fatalf("apps = %+v, %v", apps, err)
	}
	f.FailOp("ListUploads", "NotFoundException", "no project")
	if _, err := c.Apps(ctx); Classify(err).Kind != cloudprov.NotFound {
		t.Fatalf("err = %v", err)
	}
	f.FailOp("ListProjects", "ArgumentException", "x")
	c.projectARN = ""
	if _, err := c.Apps(ctx); err == nil {
		t.Fatal("project failure ignored")
	}
}
