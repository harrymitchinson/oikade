package host

import (
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
// deploy finishes whether or not the caller stays to hear how.
func (s *apiServer) deployUpload(w http.ResponseWriter, r *http.Request) {
	s.log.Info("Received API request", "type", api.TypeDeploy, "client", callerOf(r.Context()).name)
	allow, _ := strconv.ParseBool(r.URL.Query().Get(api.AllowSafetyRemovalParam))
	path, err := s.spool(w, r)
	if err != nil {
		s.log.Info("Dropped deploy upload", "err", err)
		respond(s.log, w, http.StatusBadRequest, api.Response{Error: fmt.Sprintf("receiving the binary: %v", err)})
		return
	}
	defer func() { _ = os.Remove(path) }()
	s.log.Info("Received deploy upload", "client", callerOf(r.Context()).name)
	resp := s.deploys.deploy(s.ctx, path, allow)
	respond(s.log, w, statusOf(resp), resp)
}

// spool writes the body to a file of its own in the data directory, which the caller removes. A
// body that stops or goes leaves nothing behind.
func (s *apiServer) spool(w http.ResponseWriter, r *http.Request) (string, error) {
	f, err := os.CreateTemp(filepath.Join(s.dataDir, uploadsDir), "deploy-*")
	if err != nil {
		return "", err
	}
	control := http.NewResponseController(w)
	body := &idleReader{r: http.MaxBytesReader(w, r.Body, maxUpload), control: control}
	_, err = io.Copy(f, body)
	_ = control.SetReadDeadline(time.Time{})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && r.ContentLength >= 0 {
		if info, statErr := os.Stat(f.Name()); statErr != nil || info.Size() != r.ContentLength {
			err = errors.New("the upload was cut short")
		}
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// idleReader gives each read uploadIdle, so a slow link gets as long as it keeps sending.
type idleReader struct {
	r       io.Reader
	control *http.ResponseController
}

func (i *idleReader) Read(p []byte) (int, error) {
	_ = i.control.SetReadDeadline(time.Now().Add(uploadIdle))
	return i.r.Read(p)
}
