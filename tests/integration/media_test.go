package integration

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Phase 9 - Media: upload, serving, deletion, and moderation of uploaded
// files (docs/08 §22, docs/09 §27, docs/11 §28–§29).
//
// The properties that matter most are adversarial: nothing client-declared
// about a file is trusted (bytes are sniffed), user input never reaches a
// storage path, a deleted object is unreachable through the API whatever the
// storage did, and ownership of the ATTACHMENT target is enforced by the
// owning domain - a cover upload for someone else's fiction fails exactly
// like editing it would.

// pngBytes is a complete, valid 1×1 transparent PNG.
var pngBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// jpegBytes carries the JPEG signature http.DetectContentType keys on.
var jpegBytes = append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x11}, 64)...)

// mediaBody is the decoded shape of one media resource.
type mediaBody struct {
	ID               string  `json:"id"`
	URL              string  `json:"url"`
	MediaType        string  `json:"media_type"`
	MimeType         string  `json:"mime_type"`
	SizeBytes        int64   `json:"size_bytes"`
	OriginalFilename *string `json:"original_filename"`
}

// uploadMedia performs a multipart upload as the given session. Empty fields
// are omitted from the form.
func (e *authEnv) uploadMedia(
	t *testing.T, w writer, purpose, novelRef, filename string, contents []byte,
) apiResponse {
	t.Helper()

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if purpose != "" {
		if err := form.WriteField("purpose", purpose); err != nil {
			t.Fatalf("write purpose: %v", err)
		}
	}
	if novelRef != "" {
		if err := form.WriteField("novel", novelRef); err != nil {
			t.Fatalf("write novel: %v", err)
		}
	}
	if contents != nil {
		part, err := form.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(contents); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/media", &buf)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Origin", testOrigin)
	for _, c := range w.authCookies() {
		r.AddCookie(c)
	}
	if w.csrfToken != "" {
		r.Header.Set("X-CSRF-Token", w.csrfToken)
	}

	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	return apiResponse{status: rec.Code, body: rec.Body.Bytes(), header: rec.Header()}
}

// servePath extracts the API-relative /media/... path from a stored URL.
func servePath(t *testing.T, url string) string {
	t.Helper()
	idx := strings.Index(url, "/media/")
	if idx < 0 {
		t.Fatalf("URL %q does not contain a /media/ path", url)
	}
	return url[idx:]
}

// fetchFile requests a serve path with no credentials.
func (e *authEnv) fetchFile(t *testing.T, path string) apiResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, r)
	return apiResponse{status: rec.Code, body: rec.Body.Bytes(), header: rec.Header()}
}

// ---------------------------------------------------------------------------
// Avatars
// ---------------------------------------------------------------------------

func TestMedia_AvatarLifecycle(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	user := writer{webSession: env.registerWeb(t)}

	res := env.uploadMedia(t, user, "avatar", "", "รูปโปรไฟล์.png", pngBytes)
	if res.status != http.StatusCreated {
		t.Fatalf("avatar upload status = %d. body: %s", res.status, res.body)
	}
	first := dataOf[mediaBody](t, res)
	if first.MediaType != "avatar" || first.MimeType != "image/png" ||
		first.SizeBytes != int64(len(pngBytes)) {
		t.Fatalf("avatar view = %+v", first)
	}
	if first.OriginalFilename == nil || *first.OriginalFilename != "รูปโปรไฟล์.png" {
		t.Fatalf("original filename not kept as metadata: %+v", first.OriginalFilename)
	}
	if !strings.Contains(first.URL, "/media/avatar/") || strings.Contains(first.URL, "รูป") {
		t.Fatalf("object URL must be generated, never the client filename: %q", first.URL)
	}

	// The file serves publicly, byte-identical, with the sniffed type.
	file := env.fetchFile(t, servePath(t, first.URL))
	if file.status != http.StatusOK || !bytes.Equal(file.body, pngBytes) {
		t.Fatalf("serve status = %d, %d bytes", file.status, len(file.body))
	}
	if ct := file.header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("served Content-Type = %q", ct)
	}
	if cc := file.header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("served Cache-Control = %q", cc)
	}

	// The profile now points at it (docs/08 §6.2 avatar_url).
	me := env.asOwner(t, user, http.MethodGet, "/api/v1/auth/me")
	if !strings.Contains(string(me.body), servePath(t, first.URL)) {
		t.Fatalf("avatar_url not attached to the profile: %s", me.body)
	}

	// Replacing = a NEW object under a NEW key; the profile moves, the old
	// object stays until its owner deletes it.
	res = env.uploadMedia(t, user, "avatar", "", "ใหม่.jpg", jpegBytes)
	if res.status != http.StatusCreated {
		t.Fatalf("second avatar upload status = %d. body: %s", res.status, res.body)
	}
	second := dataOf[mediaBody](t, res)
	if second.URL == first.URL {
		t.Fatal("replacement reused the object key")
	}
	if second.MimeType != "image/jpeg" {
		t.Fatalf("second avatar mime = %q, want the SNIFFED image/jpeg", second.MimeType)
	}
	me = env.asOwner(t, user, http.MethodGet, "/api/v1/auth/me")
	if !strings.Contains(string(me.body), servePath(t, second.URL)) ||
		strings.Contains(string(me.body), servePath(t, first.URL)) {
		t.Fatalf("profile did not move to the new avatar: %s", me.body)
	}
	if env.fetchFile(t, servePath(t, first.URL)).status != http.StatusOK {
		t.Fatal("replacement must not delete the old object")
	}

	// The owner withdraws the old file: idempotent 204s, then the URL is gone.
	if res := env.asOwner(t, user, http.MethodDelete, "/api/v1/media/"+first.ID); res.status != http.StatusNoContent {
		t.Fatalf("delete status = %d. body: %s", res.status, res.body)
	}
	if res := env.asOwner(t, user, http.MethodDelete, "/api/v1/media/"+first.ID); res.status != http.StatusNoContent {
		t.Fatalf("repeat delete status = %d, want 204 (idempotent)", res.status)
	}
	if env.fetchFile(t, servePath(t, first.URL)).status != http.StatusNotFound {
		t.Fatal("deleted media still serves")
	}
	if env.fetchFile(t, servePath(t, second.URL)).status != http.StatusOK {
		t.Fatal("deleting one object must not affect another")
	}
}

// ---------------------------------------------------------------------------
// Covers - attachment authorization belongs to the novels domain
// ---------------------------------------------------------------------------

func TestMedia_CoverOwnership(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	private := env.createNovel(t, author, createNovelBody(uniqueName(t, "Draft "), nil))
	public := env.publishedNovel(t, author, nil)
	stranger := writer{webSession: env.registerWeb(t)}

	// The owner sets a cover on a private draft - drafting needs no
	// verification gate and no visibility change.
	res := env.uploadMedia(t, author, "novel_cover", private.ID, "cover.png", pngBytes)
	if res.status != http.StatusCreated {
		t.Fatalf("cover upload status = %d. body: %s", res.status, res.body)
	}
	cover := dataOf[mediaBody](t, res)

	shown := env.asOwner(t, author, http.MethodGet, "/api/v1/novels/"+private.ID)
	if !strings.Contains(string(shown.body), servePath(t, cover.URL)) {
		t.Fatalf("cover_url not attached: %s", shown.body)
	}

	// A stranger uploading a cover for someone else's PRIVATE fiction gets
	// the same 404 reading it would (docs/11 §21) - and no object appears.
	res = env.uploadMedia(t, stranger, "novel_cover", private.ID, "evil.png", pngBytes)
	if res.status != http.StatusNotFound {
		t.Fatalf("stranger cover on private novel status = %d, want 404", res.status)
	}
	// On a PUBLIC fiction the refusal is an honest 403.
	res = env.uploadMedia(t, stranger, "novel_cover", public.ID, "evil.png", pngBytes)
	if res.status != http.StatusForbidden {
		t.Fatalf("stranger cover on public novel status = %d, want 403", res.status)
	}

	// A cover upload without its fiction is a validation error.
	res = env.uploadMedia(t, author, "novel_cover", "", "cover.png", pngBytes)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("cover without novel status = %d, want 422", res.status)
	}
}

// ---------------------------------------------------------------------------
// Upload validation - the untrusted-input boundary (docs/11 §28)
// ---------------------------------------------------------------------------

func TestMedia_UploadValidation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	user := writer{webSession: env.registerWeb(t)}

	// A guest cannot upload at all.
	guest := writer{}
	if res := env.uploadMedia(t, guest, "avatar", "", "a.png", pngBytes); res.status != http.StatusUnauthorized {
		t.Fatalf("guest upload status = %d, want 401", res.status)
	}

	// A cookie session without its CSRF token is rejected (docs/11 §22).
	noCSRF := writer{webSession: user.webSession}
	noCSRF.csrfToken = ""
	if res := env.uploadMedia(t, noCSRF, "avatar", "", "a.png", pngBytes); res.status != http.StatusForbidden {
		t.Fatalf("upload without CSRF status = %d, want 403", res.status)
	}

	rejected := []struct {
		name     string
		purpose  string
		filename string
		contents []byte
	}{
		{"missing file", "avatar", "", nil},
		{"empty file", "avatar", "a.png", []byte{}},
		{"text disguised as png", "avatar", "a.png", []byte("just text, not an image")},
		{"svg (script risk)", "avatar", "a.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		{"executable", "avatar", "a.png", append([]byte("MZ"), bytes.Repeat([]byte{0x90}, 64)...)},
		{"pdf", "avatar", "a.png", []byte("%PDF-1.4 not an image")},
		{"unknown purpose", "banner", "a.png", pngBytes},
		{"vocabulary type without a surface yet", "community_image", "a.png", pngBytes},
		{"missing purpose", "", "a.png", pngBytes},
	}
	for _, tc := range rejected {
		res := env.uploadMedia(t, user, tc.purpose, "", tc.filename, tc.contents)
		if res.status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422. body: %s", tc.name, res.status, res.body)
		}
	}

	// Oversize: a real PNG signature followed by more bytes than the cap.
	huge := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0xaa}, testMediaMaxBytes)...)
	res := env.uploadMedia(t, user, "avatar", "", "big.png", huge)
	if res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload status = %d, want 413. body: %s", res.status, res.body)
	}
	if errorCodeOf(t, res) != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("oversize error code = %s", res.body)
	}

	// The sniffed type wins over the lying extension - a JPEG named .png is
	// stored as what it IS.
	res = env.uploadMedia(t, user, "avatar", "", "lie.png", jpegBytes)
	if res.status != http.StatusCreated {
		t.Fatalf("jpeg-as-png upload status = %d. body: %s", res.status, res.body)
	}
	if v := dataOf[mediaBody](t, res); v.MimeType != "image/jpeg" || !strings.HasSuffix(servePath(t, v.URL), ".jpg") {
		t.Fatalf("sniffing did not win: %+v", v)
	}
}

// ---------------------------------------------------------------------------
// Deletion authorization and the serve path
// ---------------------------------------------------------------------------

func TestMedia_DeleteAuthorizationAndServe(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := writer{webSession: env.registerWeb(t)}
	other := writer{webSession: env.registerWeb(t)}

	res := env.uploadMedia(t, owner, "avatar", "", "mine.png", pngBytes)
	uploaded := dataOf[mediaBody](t, res)

	// Having the id is not authorization (Phase 9 brief).
	if res := env.asOwner(t, other, http.MethodDelete, "/api/v1/media/"+uploaded.ID); res.status != http.StatusForbidden {
		t.Fatalf("foreign delete status = %d, want 403", res.status)
	}
	if res := env.asGuest(t, http.MethodDelete, "/api/v1/media/"+uploaded.ID); res.status != http.StatusUnauthorized {
		t.Fatalf("guest delete status = %d, want 401", res.status)
	}
	// Ghost and malformed ids answer the same 404 (docs/11 §3.4).
	if res := env.asOwner(t, owner, http.MethodDelete,
		"/api/v1/media/3f1f8de1-0000-4000-8000-000000000000"); res.status != http.StatusNotFound {
		t.Fatalf("ghost delete status = %d, want 404", res.status)
	}
	if res := env.asOwner(t, owner, http.MethodDelete, "/api/v1/media/not-a-uuid"); res.status != http.StatusNotFound {
		t.Fatalf("malformed delete status = %d, want 404", res.status)
	}

	// The serve path rejects unknown and hostile keys identically.
	for _, path := range []string{
		"/media/avatar/no-such-object.png",
		"/media/../../etc/passwd",
		"/media/avatar/..%2f..%2fsecret",
	} {
		if res := env.fetchFile(t, path); res.status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, res.status)
		}
	}
}

// ---------------------------------------------------------------------------
// Moderation - media is reportable (docs/11 §38)
// ---------------------------------------------------------------------------

func TestMedia_Moderation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := writer{webSession: env.registerWeb(t)}
	reporter := writer{webSession: env.registerWeb(t)}
	moderator := env.newModerator(t)

	res := env.uploadMedia(t, owner, "avatar", "", "reported.png", pngBytes)
	uploaded := dataOf[mediaBody](t, res)
	path := servePath(t, uploaded.URL)

	// Reportable while live; a ghost id is the usual 404.
	report := env.fileReport(t, reporter, "media", uploaded.ID, "illegal")
	if report.Status != "pending" {
		t.Fatalf("media report = %+v", report)
	}
	ghost := env.asOwner(t, reporter, http.MethodPost, "/api/v1/reports", map[string]string{
		"target_type": "media", "target_id": "3f1f8de1-0000-4000-8000-000000000000",
		"reason": "illegal",
	})
	if ghost.status != http.StatusNotFound {
		t.Fatalf("ghost media report status = %d, want 404", ghost.status)
	}

	// The staff detail snapshot shows the file's metadata and owner.
	detail := env.asOwner(t, moderator, http.MethodGet, "/api/v1/admin/reports/"+report.ID)
	if !strings.Contains(string(detail.body), "reported.png") ||
		!strings.Contains(string(detail.body), owner.username) {
		t.Fatalf("media snapshot missing metadata: %s", detail.body)
	}

	// Remove: the file stops serving; the OBJECT is kept, so restore is
	// lossless - unlike an owner delete.
	env.performAction(t, moderator, "media", uploaded.ID, "remove")
	if env.fetchFile(t, path).status != http.StatusNotFound {
		t.Fatal("moderation-removed media still serves")
	}
	res = env.asOwner(t, moderator, http.MethodPost, "/api/v1/admin/moderation/actions",
		map[string]string{"target_type": "media", "target_id": uploaded.ID, "action": "remove"})
	if res.status != http.StatusConflict {
		t.Fatalf("re-remove status = %d, want 409", res.status)
	}

	env.performAction(t, moderator, "media", uploaded.ID, "restore")
	file := env.fetchFile(t, path)
	if file.status != http.StatusOK || !bytes.Equal(file.body, pngBytes) {
		t.Fatalf("restored media does not serve intact: status %d", file.status)
	}

	// The owner heard about it - actor-less, entity media.
	items := env.awaitNotifications(t, owner, 2)
	moderationSeen := 0
	for _, item := range items {
		if item.Type != "moderation" {
			continue
		}
		moderationSeen++
		if item.Actor != nil || item.EntityType == nil || *item.EntityType != "media" {
			t.Fatalf("moderation notification shape: %+v", item)
		}
	}
	if moderationSeen != 2 {
		t.Fatalf("owner holds %d moderation notifications, want 2 (types %v)",
			moderationSeen, typesOf(items))
	}
}

// serveIsolation: uploads must not weaken the small global body cap for the
// rest of the API - an ordinary JSON endpoint still refuses a megabyte body.
func TestMedia_GlobalBodyLimitUnchanged(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	user := writer{webSession: env.registerWeb(t)}

	big := strings.Repeat("ก", 1<<21) // ~6 MiB of UTF-8, far over the 1 MiB cap
	res := env.do(t, apiRequest{
		method:  http.MethodPost,
		path:    "/api/v1/community/posts",
		body:    map[string]string{"content": big},
		cookies: user.authCookies(),
		csrf:    user.csrfToken,
	})
	// The cap truncates the body mid-read, which the JSON bind layer has
	// always reported as its 400 - the pre-Phase-9 contract. What matters
	// here is that the request is REFUSED: the raised media limit must not
	// have leaked onto ordinary endpoints.
	if res.status != http.StatusBadRequest && res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized JSON body status = %d, want a refusal", res.status)
	}
}
