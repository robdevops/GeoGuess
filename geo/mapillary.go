package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MapillaryURL is Mapillary's Graph API.
const MapillaryURL = "https://graph.mapillary.com"

// mapillaryFields are the fields asked for of each picked image.
const mapillaryFields = "id,computed_geometry,geometry,captured_at,camera_type,is_pano,quality_score,width,height,sequence,creator,thumb_2048_url"

// The survey of a box: its light fields, the most images it lists, how
// long it may take before the box is split, and the smallest box (by half
// its side, in degrees) it splits down to.
const (
	surveyFields  = "id,sequence,computed_geometry"
	surveyLimit   = 1000
	surveyTimeout = 35 * time.Second
	minSurveyHalf = 0.0025
)

// mapillaryPicks is the most images a box offers, one from each of as many
// sequences; mapillaryBatch is the most IDs asked for in one details call.
const (
	mapillaryPicks = 40
	mapillaryBatch = 50
)

// maxMapillaryAnswer caps one answer.
const maxMapillaryAnswer = 4 << 20

// Mapillary searches Mapillary's Graph API v4. Its photos are CC BY-SA 4.0.
type Mapillary struct {
	url    string
	token  string
	client *http.Client
	survey time.Duration // surveyTimeout; shorter in tests
	batch  int           // mapillaryBatch; smaller in tests
}

// NewMapillary is a Mapillary client for the API at base (MapillaryURL),
// sending token (MLY|…) as "Authorization: OAuth <token>": Mapillary's
// answers to a Bearer token hold no data.
func NewMapillary(base, token string, client *http.Client) *Mapillary {
	return &Mapillary{url: base, token: token, client: client, survey: surveyTimeout, batch: mapillaryBatch}
}

// Name is "Mapillary".
func (m *Mapillary) Name() string { return "Mapillary" }

type mapillaryImage struct {
	ID               string        `json:"id"`
	ComputedGeometry *geoJSONPoint `json:"computed_geometry"`
	Geometry         *geoJSONPoint `json:"geometry"`
	CapturedAt       int64         `json:"captured_at"` // ms since the epoch, by the camera's clock
	CameraType       string        `json:"camera_type"`
	IsPano           bool          `json:"is_pano"`
	QualityScore     *float64      `json:"quality_score"` // null is unknown, not bad
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	Sequence         string        `json:"sequence"`
	Creator          struct {
		Username string `json:"username"`
	} `json:"creator"`
	Thumb string `json:"thumb_2048_url"`
}

// mapillaryError is the error a Graph API answer may hold.
type mapillaryError struct {
	Code int `json:"code"`
}

type mapillaryAnswer struct {
	Data  []mapillaryImage `json:"data"`
	Error *mapillaryError  `json:"error"`
}

// Candidates surveys b, which must be under 0.01 deg², for its images' IDs
// and sequences, picks one image at random from each sequence, up to
// mapillaryPicks of them in a random order, and asks for those images'
// details. A survey that fails with a server error or takes too long (an
// area too dense to list) surveys a random quarter of the box instead,
// down to a box minSurveyHalf across each way from its centre.
func (m *Mapillary) Candidates(ctx context.Context, b BBox) ([]Candidate, error) {
	images, err := m.surveyBox(ctx, b)
	if err != nil {
		return nil, err
	}
	ids := pickPerSequence(images, mapillaryPicks)
	out := make([]Candidate, 0, len(ids))
	for len(ids) > 0 {
		batch := ids[:min(len(ids), m.batch)]
		ids = ids[len(batch):]
		details, err := m.details(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, id := range batch {
			if im, ok := details[id]; ok {
				if c, ok := im.candidate(); ok {
					out = append(out, c)
				}
			}
		}
	}
	return out, nil
}

// surveyBox lists b's images by their light fields, splitting b into a
// random quarter while Mapillary can't list it. An empty answer is asked
// again once, since Mapillary sometimes leaves out images it has.
func (m *Mapillary) surveyBox(ctx context.Context, b BBox) ([]mapillaryImage, error) {
	for {
		images, err := m.surveyOnce(ctx, b)
		if err == nil && len(images) == 0 {
			images, err = m.surveyOnce(ctx, b)
		}
		if err == nil || !tooDense(err) || ctx.Err() != nil {
			return images, err
		}
		half := max(b.MaxLat-b.MinLat, b.MaxLon-b.MinLon) / 4 // the quarter's
		if half < minSurveyHalf*0.999 {
			return nil, err
		}
		b = b.quarter(rand.IntN(4))
	}
}

// errSurveyTimeout is a survey that took longer than its own timeout.
var errSurveyTimeout = errors.New("Mapillary: survey timed out")

// tooDense reports whether err is Mapillary failing to list a box: a
// server error or a survey that timed out.
func tooDense(err error) bool {
	se := StatusError{}
	return errors.Is(err, errSurveyTimeout) || (errors.As(err, &se) && se.Code >= 500)
}

// surveyOnce is one survey of b, given at most m.survey.
func (m *Mapillary) surveyOnce(ctx context.Context, b BBox) ([]mapillaryImage, error) {
	sctx, cancel := context.WithTimeout(ctx, m.survey)
	defer cancel()
	q := url.Values{"bbox": {b.String()}, "fields": {surveyFields}, "limit": {strconv.Itoa(surveyLimit)}}
	var a mapillaryAnswer
	err := m.get(sctx, "/images?"+q.Encode(), &a)
	if err != nil && ctx.Err() == nil && sctx.Err() != nil {
		return nil, errSurveyTimeout
	}
	if err == nil && a.Error != nil {
		err = fmt.Errorf("%s: unreadable answer", m.Name())
	}
	return a.Data, err
}

// details is the full fields of the images ids, by ID; an ID Mapillary
// leaves out is missing.
func (m *Mapillary) details(ctx context.Context, ids []string) (map[string]mapillaryImage, error) {
	q := url.Values{"ids": {strings.Join(ids, ",")}, "fields": {mapillaryFields}}
	var a map[string]json.RawMessage
	if err := m.get(ctx, "/?"+q.Encode(), &a); err != nil {
		return nil, err
	}
	if _, ok := a["error"]; ok {
		return nil, fmt.Errorf("%s: unreadable answer", m.Name())
	}
	out := make(map[string]mapillaryImage, len(a))
	for id, raw := range a {
		var im mapillaryImage
		if json.Unmarshal(raw, &im) == nil && im.ID == id {
			out[id] = im
		}
	}
	return out, nil
}

// get is one call to the Graph API at path, decoding its answer into v. A
// rate limit (Mapillary's error code 4, or a 429 or 403) is ErrSlowDown.
func (m *Mapillary) get(ctx context.Context, path string, v any) error {
	r := request{client: m.client, source: m.Name(), url: m.url + path, auth: "OAuth " + m.token, limit: maxMapillaryAnswer}
	body, err := r.get(ctx)
	var e struct {
		Error *mapillaryError `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != nil && e.Error.Code == 4 {
		return ErrSlowDown // "application request limit reached"
	}
	if se := (StatusError{}); errors.As(err, &se) && (se.Code == http.StatusTooManyRequests || se.Code == http.StatusForbidden) {
		return ErrSlowDown
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(body, v) != nil {
		return fmt.Errorf("%s: unreadable answer", m.Name())
	}
	return nil
}

// pickPerSequence is the ID of one image chosen at random from each
// sequence in images (an image with no sequence is its own), at most n of
// them, in a random order.
func pickPerSequence(images []mapillaryImage, n int) []string {
	bySeq := map[string][]string{}
	var seqs []string
	for _, im := range images {
		if im.ID == "" {
			continue
		}
		seq := im.Sequence
		if seq == "" {
			seq = "image:" + im.ID
		}
		if _, ok := bySeq[seq]; !ok {
			seqs = append(seqs, seq)
		}
		bySeq[seq] = append(bySeq[seq], im.ID)
	}
	ids := make([]string, len(seqs))
	for i, seq := range seqs {
		ids[i] = bySeq[seq][rand.IntN(len(bySeq[seq]))]
	}
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	return ids[:min(len(ids), n)]
}

// candidate is im as a Candidate, if it has an ID and a position.
func (im mapillaryImage) candidate() (Candidate, bool) {
	raw, rawOK := im.Geometry.point()
	at, ok := im.ComputedGeometry.point()
	if !ok {
		at, ok = raw, rawOK
	}
	if !rawOK {
		raw = at
	}
	if im.ID == "" || !ok {
		return Candidate{}, false
	}
	c := Candidate{
		ID:       im.ID,
		Source:   "Mapillary",
		Creator:  im.Creator.Username,
		Licence:  "CC BY-SA 4.0",
		Link:     "https://www.mapillary.com/app/?pKey=" + url.QueryEscape(im.ID),
		At:       at,
		Raw:      raw,
		Pano:     im.IsPano || im.CameraType == "spherical" || im.CameraType == "equirectangular",
		Fisheye:  im.CameraType == "fisheye",
		Quality:  -1,
		Width:    im.Width,
		Height:   im.Height,
		Sequence: im.Sequence,
		image:    im.Thumb,
	}
	if im.CapturedAt > 0 {
		c.Captured = time.UnixMilli(im.CapturedAt).UTC()
	}
	if im.QualityScore != nil {
		c.Quality = *im.QualityScore
	}
	return c, true
}

// Fetch downloads c's 2048-px thumbnail. The URL a search gave is signed and
// expires, so a candidate without one, or whose URL has expired, gets a fresh
// one by ID.
func (m *Mapillary) Fetch(ctx context.Context, c Candidate) ([]byte, error) {
	if c.image != "" {
		data, err := request{client: m.client, source: m.Name(), url: c.image, limit: maxPhoto}.get(ctx)
		if err == nil {
			return data, nil
		}
		if se := (StatusError{}); !errors.As(err, &se) || (se.Code != http.StatusForbidden && se.Code != http.StatusNotFound && se.Code != http.StatusGone) {
			return nil, err
		}
	}
	var fresh struct {
		Thumb string `json:"thumb_2048_url"`
	}
	q := url.Values{"fields": {"thumb_2048_url"}}
	r := request{client: m.client, source: m.Name(), url: m.url + "/" + url.PathEscape(c.ID) + "?" + q.Encode(), auth: "OAuth " + m.token, limit: 64 << 10}
	if err := r.getJSON(ctx, &fresh); err != nil {
		return nil, err
	}
	if fresh.Thumb == "" {
		return nil, fmt.Errorf("%s: no image", m.Name())
	}
	return request{client: m.client, source: m.Name(), url: fresh.Thumb, limit: maxPhoto}.get(ctx)
}
