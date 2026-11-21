package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"oikade.dev/internal/host/api"
)

const (
	// uploadsDir holds deploys on their way in, in the data directory.
	uploadsDir = "uploads"
	maxUpload  = 1 << 30
	maxVars    = 1 << 20
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

// deployUpload takes a deploy's values and binary as the request body, then deploys them. Once it's
// all here, the deploy finishes whether or not the caller stays to hear how. Until then, the caller
// going, its client being revoked or the host stopping ends it, and nothing is deployed.
func (s *apiServer) deployUpload(w http.ResponseWriter, r *http.Request) {
	s.log.Info("Received API request", "type", api.TypeDeploy, "client", callerOf(r.Context()).name)
	boundary, ok := deployBoundary(r.Header.Get("Content-Type"))
	if !ok {
		respond(s.log, w, http.StatusUnsupportedMediaType, api.Response{
			Error: "a deploy's body is your values, then the binary, as " + api.DeployContentType +
				": this client is from another version",
		})
		return
	}
	if r.ContentLength > maxUpload {
		respond(s.log, w, http.StatusRequestEntityTooLarge, api.Response{
			Error: fmt.Sprintf("a binary can be at most %d MiB", maxUpload>>20),
		})
		return
	}
	allow, _ := strconv.ParseBool(r.URL.Query().Get(api.AllowSafetyParam))
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	vars, path, err := s.spool(ctx, w, r, boundary)
	if err != nil {
		s.log.Info("Dropped deploy upload", "err", err)
		respond(s.log, w, http.StatusBadRequest, api.Response{Error: fmt.Sprintf("receiving the deploy: %v", err)})
		return
	}
	// Gone once the deploy has it, which moves it, or not.
	defer func() { _ = os.Remove(path) }()
	s.log.Info("Received deploy upload", "client", callerOf(r.Context()).name, "vars", len(vars), "keepVars", vars == nil)
	resp := s.deploys.deploy(s.ctx, path, vars, allow)
	respond(s.log, w, statusOf(resp), resp)
}

// deployBoundary is the boundary between a deploy's parts, if contentType is one's.
func deployBoundary(contentType string) (string, bool) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != api.DeployContentType || params["boundary"] == "" {
		return "", false
	}
	return params["boundary"], true
}

// spool reads the body: the values, and the binary, which it writes to a file of its own in the
// data directory, which the caller removes. A body that stops, or a ctx that ends first, leaves
// nothing behind.
func (s *apiServer) spool(ctx context.Context, w http.ResponseWriter, r *http.Request, boundary string) (
	map[string]string, string, error,
) {
	control := http.NewResponseController(w)
	// An ending ctx wakes a read that's waiting.
	stop := context.AfterFunc(ctx, func() { _ = control.SetReadDeadline(time.Now()) })
	body := &idleReader{r: http.MaxBytesReader(w, r.Body, maxUpload), control: control, ctx: ctx}
	vars, path, err := s.receive(multipart.NewReader(body, boundary))
	if !stop() || ctx.Err() != nil {
		err = errors.Join(err, context.Cause(ctx))
	}
	_ = control.SetReadDeadline(time.Time{})
	if err != nil {
		if path != "" {
			_ = os.Remove(path)
		}
		return nil, "", err
	}
	return vars, path, nil
}

// receive reads a deploy's parts in order: the values, if it sends them, then the binary, into a
// file whose path it returns even if it fails after making it. Values it doesn't send are nil, so
// that the host keeps its own. Its errors never quote a value.
func (s *apiServer) receive(parts *multipart.Reader) (map[string]string, string, error) {
	part, err := nextPart(parts, api.DeployVarsPart, api.DeployBinaryPart)
	if err != nil {
		return nil, "", err
	}
	var vars map[string]string
	if part.FormName() == api.DeployVarsPart {
		if vars, err = readVars(part); err != nil {
			return nil, "", err
		}
		if part, err = nextPart(parts, api.DeployBinaryPart); err != nil {
			return nil, "", err
		}
	}
	f, err := os.CreateTemp(filepath.Join(s.dataDir, uploadsDir), "deploy-*")
	if err != nil {
		return nil, "", err
	}
	_, err = io.Copy(f, part)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, f.Name(), cutShort(err)
	}
	// Only the closing boundary says the binary is all here: that's a bare EOF, and a body that
	// ends before it is a wrapped one.
	if _, err := parts.NextPart(); err != io.EOF {
		if err == nil {
			err = errors.New("the deploy has a part after the binary")
		}
		return nil, f.Name(), cutShort(err)
	}
	return vars, f.Name(), nil
}

// nextPart is the next part, which must be called one of names. A body with none names the last.
func nextPart(parts *multipart.Reader, names ...string) (*multipart.Part, error) {
	part, err := parts.NextPart()
	switch {
	case err == io.EOF: // The closing boundary, and not a body cut short.
		return nil, fmt.Errorf("the deploy has no %s", names[len(names)-1])
	case err != nil:
		return nil, cutShort(err)
	case !slices.Contains(names, part.FormName()):
		return nil, fmt.Errorf("the deploy has %q where its %s should be", part.FormName(), strings.Join(names, " or "))
	}
	return part, nil
}

// readVars reads the values' part: a JSON object of names to strings, which null leaves empty.
func readVars(part *multipart.Part) (map[string]string, error) {
	raw, err := io.ReadAll(io.LimitReader(part, maxVars+1))
	if err != nil {
		return nil, cutShort(err)
	}
	if len(raw) > maxVars {
		return nil, fmt.Errorf("the values can be at most %d MiB", maxVars>>20)
	}
	var vars map[string]string
	if json.Unmarshal(raw, &vars) != nil {
		return nil, errors.New("the values aren't a JSON object of strings")
	}
	if vars == nil {
		vars = map[string]string{}
	}
	return vars, nil
}

// cutShort explains a body that ended before its last boundary.
func cutShort(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return errors.New("the upload was cut short")
	}
	return err
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
