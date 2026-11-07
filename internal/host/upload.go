package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/harrymitchinson/oikade/internal/host/api"
)

const (
	// uploadsDir holds deploys on their way in, in the data directory.
	uploadsDir = "uploads"
	// maxUpload bounds a deployed binary.
	maxUpload = 1 << 30
	// uploadIdle is how long an upload may send nothing, on a slow link.
	uploadIdle = 30 * time.Second
)

// clearUploads removes what a host that stopped mid-upload left.
func clearUploads(dataDir string) error {
	dir := filepath.Join(dataDir, uploadsDir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

// deployUpload takes a deploy's binary as the request body, then deploys it. Once it's all here, the
// deploy finishes whether or not the caller stays to hear how. Until then, the caller going, its
// client being revoked or the host stopping ends it, and nothing is deployed.
func (s *apiServer) deployUpload(w http.ResponseWriter, r *http.Request) {
	s.log.Info("Received API request", "type", api.TypeDeploy, "client", callerOf(r.Context()).name)
	if r.Header.Get("Content-Type") != api.BinaryContentType {
		respond(s.log, w, http.StatusUnsupportedMediaType, api.Response{
			Error: "a deploy's body is the binary, as " + api.BinaryContentType + ": this client is from another version",
		})
		return
	}
	if r.ContentLength > maxUpload {
		respond(s.log, w, http.StatusRequestEntityTooLarge, api.Response{
			Error: fmt.Sprintf("a binary can be at most %d MiB", maxUpload>>20),
		})
		return
	}
	allow, _ := strconv.ParseBool(r.URL.Query().Get(api.AllowSafetyRemovalParam))
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	path, err := s.spool(ctx, w, r)
	if err != nil {
		s.log.Info("Dropped deploy upload", "err", err)
		respond(s.log, w, http.StatusBadRequest, api.Response{Error: fmt.Sprintf("receiving the binary: %v", err)})
		return
	}
	// Gone once the deploy has it, which moves it, or not.
	defer func() { _ = os.Remove(path) }()
	s.log.Info("Received deploy upload", "client", callerOf(r.Context()).name)
	resp := s.deploys.deploy(s.ctx, path, allow)
	respond(s.log, w, statusOf(resp), resp)
}

// spool writes the body to a file of its own in the data directory, which the caller removes. A
// body that stops, or a ctx that ends first, leaves nothing behind.
func (s *apiServer) spool(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, error) {
	f, err := os.CreateTemp(filepath.Join(s.dataDir, uploadsDir), "deploy-*")
	if err != nil {
		return "", err
	}
	control := http.NewResponseController(w)
	// An ending ctx wakes a read that's waiting.
	stop := context.AfterFunc(ctx, func() { _ = control.SetReadDeadline(time.Now()) })
	body := &idleReader{r: http.MaxBytesReader(w, r.Body, maxUpload), control: control, ctx: ctx}
	n, err := io.Copy(f, body)
	if !stop() || ctx.Err() != nil {
		err = errors.Join(err, context.Cause(ctx))
	}
	_ = control.SetReadDeadline(time.Time{})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && r.ContentLength >= 0 && n != r.ContentLength {
		err = errors.New("the upload was cut short")
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// idleReader gives each read uploadIdle, so a slow link gets as long as it keeps sending, until ctx
// ends.
type idleReader struct {
	r       io.Reader
	control *http.ResponseController
	ctx     context.Context
}

func (i *idleReader) Read(p []byte) (int, error) {
	if err := i.ctx.Err(); err != nil {
		return 0, err
	}
	_ = i.control.SetReadDeadline(time.Now().Add(uploadIdle))
	return i.r.Read(p)
}
