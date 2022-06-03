// SPDX-License-Identifier: AGPL-3.0-only

package compactor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/objstore"

	"github.com/grafana/dskit/tenant"
	"github.com/grafana/regexp"

	"github.com/grafana/mimir/pkg/storage/bucket"
	mimir_tsdb "github.com/grafana/mimir/pkg/storage/tsdb"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

// HandleBlockUpload handles requests for starting or completing block uploads.
//
// The query parameter uploadComplete (true or false, default false) controls whether the
// upload should be completed or not.
//
// Starting the uploading of a block means to upload meta.json and verify that the upload can go ahead.
// In practice this means to check that the (complete) block isn't already in block storage,
// and that meta.json is valid.
func (c *MultitenantCompactor) HandleBlockUpload(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}
	bULID, err := ulid.Parse(blockID)
	if err != nil {
		http.Error(w, "invalid block ID", http.StatusBadRequest)
		return
	}
	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	logger := util_log.WithContext(ctx, c.logger)
	logger = log.With(logger, "block", blockID)

	if r.URL.Query().Get("uploadComplete") == "true" {
		c.completeBlockUpload(ctx, w, r, logger, tenantID, bULID)
	} else {
		c.createBlockUpload(ctx, w, r, logger, tenantID, bULID)
	}
}

func (c *MultitenantCompactor) createBlockUpload(ctx context.Context, w http.ResponseWriter, r *http.Request,
	logger log.Logger, tenantID string, blockID ulid.ULID) {
	level.Debug(logger).Log("msg", "starting block upload")

	bkt := bucket.NewUserBucketClient(string(tenantID), c.bucketClient, c.cfgProvider)

	exists, err := bkt.Exists(ctx, path.Join(blockID.String(), "meta.json"))
	if err != nil {
		level.Error(logger).Log("msg", "failed to check existence of meta.json in object storage",
			"err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if exists {
		level.Debug(logger).Log("msg", "complete block already exists in object storage")
		http.Error(w, "block already exists in object storage", http.StatusConflict)
		return
	}

	dec := json.NewDecoder(r.Body)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	if err := c.sanitizeMeta(logger, tenantID, blockID, &meta); err != nil {
		var eBadReq errBadRequest
		if errors.As(err, &eBadReq) {
			level.Warn(logger).Log("msg", eBadReq.message)
			http.Error(w, eBadReq.message, http.StatusBadRequest)
			return
		}

		level.Error(logger).Log("msg", "failed to sanitize meta.json", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := c.uploadMeta(ctx, logger, meta, blockID, tenantID, "meta.json.temp", bkt); err != nil {
		level.Error(logger).Log("msg", "failed to upload meta.json", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// UploadBlockFile handles requests for uploading block files.
//
// It takes the mandatory query parameter "path", specifying the file's destination path.
func (c *MultitenantCompactor) UploadBlockFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockID := vars["block"]
	if blockID == "" {
		http.Error(w, "missing block ID", http.StatusBadRequest)
		return
	}
	_, err := ulid.Parse(blockID)
	if err != nil {
		http.Error(w, "invalid block ID", http.StatusBadRequest)
		return
	}
	pth := r.URL.Query().Get("path")
	if pth == "" {
		http.Error(w, "missing or invalid file path", http.StatusBadRequest)
		return
	}

	tenantID, ctx, err := tenant.ExtractTenantIDFromHTTPRequest(r)
	if err != nil {
		http.Error(w, "invalid tenant ID", http.StatusBadRequest)
		return
	}

	logger := util_log.WithContext(ctx, c.logger)
	logger = log.With(logger, "block", blockID)

	if path.Base(pth) == "meta.json" {
		http.Error(w, "meta.json is not allowed", http.StatusBadRequest)
		return
	}

	rePath := regexp.MustCompile(`^(index|chunks/\d{6})$`)
	if !rePath.MatchString(pth) {
		http.Error(w, fmt.Sprintf("invalid path: %q", pth), http.StatusBadRequest)
		return
	}

	if r.Body == nil || r.ContentLength == 0 {
		http.Error(w, "file cannot be empty", http.StatusBadRequest)
		return
	}

	bkt := bucket.NewUserBucketClient(string(tenantID), c.bucketClient, c.cfgProvider)

	metaPath := path.Join(blockID, "meta.json.temp")
	exists, err := bkt.Exists(ctx, metaPath)
	if err != nil {
		level.Error(logger).Log("msg", "failed to check existence of meta.json.temp in object storage",
			"err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, fmt.Sprintf("upload of block %s not started yet", blockID), http.StatusBadRequest)
		return
	}

	rdr, err := bkt.Get(ctx, metaPath)
	if err != nil {
		level.Error(logger).Log("msg", "failed to download meta.json.temp from object storage",
			"err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dec := json.NewDecoder(rdr)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		level.Error(logger).Log("msg", "failed to decode meta.json.temp",
			"err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// TODO: Verify that upload path and length correspond to file index

	dst := path.Join(blockID, pth)

	level.Debug(logger).Log("msg", "uploading block file to bucket", "destination", dst,
		"size", r.ContentLength)
	reader := bodyReader{
		r: r,
	}
	if err := bkt.Upload(ctx, dst, reader); err != nil {
		level.Error(logger).Log("msg", "failed uploading block file to bucket",
			"destination", dst, "err", err)
		http.Error(w, "failed uploading block file to bucket", http.StatusBadGateway)
		return
	}

	level.Debug(logger).Log("msg", "finished uploading block file to bucket",
		"path", pth)

	w.WriteHeader(http.StatusOK)
}

func (c *MultitenantCompactor) completeBlockUpload(ctx context.Context, w http.ResponseWriter, r *http.Request,
	logger log.Logger, tenantID string, blockID ulid.ULID) {
	level.Debug(logger).Log("msg", "received request to complete block upload", "content_length", r.ContentLength)

	bkt := bucket.NewUserBucketClient(tenantID, c.bucketClient, c.cfgProvider)

	rdr, err := bkt.Get(ctx, path.Join(blockID.String(), "meta.json.temp"))
	if err != nil {
		level.Error(logger).Log("msg", "failed to download meta.json.temp from object storage",
			"err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	dec := json.NewDecoder(rdr)
	var meta metadata.Meta
	if err := dec.Decode(&meta); err != nil {
		level.Error(logger).Log("msg", "failed to decode meta.json", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	level.Debug(logger).Log("msg", "completing block upload", "files", len(meta.Thanos.Files))

	// Upload meta.json so block is considered complete
	if err := c.uploadMeta(ctx, logger, meta, blockID, tenantID, "meta.json", bkt); err != nil {
		level.Error(logger).Log("msg", "failed to upload meta.json", "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := bkt.Delete(ctx, path.Join(blockID.String(), "meta.json.temp")); err != nil {
		level.Error(logger).Log("msg", "failed to delete meta.json.temp from block in object storage",
			"err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	level.Debug(logger).Log("msg", "successfully completed block upload")

	w.WriteHeader(http.StatusOK)
}

type errBadRequest struct {
	message string
}

func (e errBadRequest) Error() string {
	return e.message
}

func (c *MultitenantCompactor) sanitizeMeta(logger log.Logger, tenantID string, blockID ulid.ULID,
	meta *metadata.Meta) error {
	if meta.Thanos.Labels == nil {
		meta.Thanos.Labels = map[string]string{}
	}

	meta.ULID = blockID
	meta.Thanos.Labels[mimir_tsdb.TenantIDExternalLabel] = tenantID

	var rejLbls []string
	for l, v := range meta.Thanos.Labels {
		switch l {
		// Preserve these labels
		case mimir_tsdb.TenantIDExternalLabel, mimir_tsdb.CompactorShardIDExternalLabel:
		// Remove unused labels
		case mimir_tsdb.IngesterIDExternalLabel, mimir_tsdb.DeprecatedShardIDExternalLabel:
			level.Debug(logger).Log("msg", "removing unused external label from meta.json",
				"label", l, "value", v)
			delete(meta.Thanos.Labels, l)
		default:
			rejLbls = append(rejLbls, l)
		}
	}

	if len(rejLbls) > 0 {
		level.Warn(logger).Log("msg", "rejecting unsupported external label(s) in meta.json",
			"labels", strings.Join(rejLbls, ","))
		return errBadRequest{message: fmt.Sprintf("unsupported external label(s): %s", strings.Join(rejLbls, ","))}
	}

	// Mark block source
	meta.Thanos.Source = "upload"

	return nil
}

func (c *MultitenantCompactor) uploadMeta(ctx context.Context, logger log.Logger, meta metadata.Meta,
	blockID ulid.ULID, tenantID, name string, bkt objstore.Bucket) error {
	dst := path.Join(blockID.String(), name)
	level.Debug(logger).Log("msg", fmt.Sprintf("uploading %s to bucket", name), "dst", dst)
	buf := bytes.NewBuffer(nil)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(meta); err != nil {
		return errors.Wrap(err, "failed to encode block metadata")
	}
	if err := bkt.Upload(ctx, dst, buf); err != nil {
		return errors.Wrapf(err, "failed uploading %s to bucket", name)
	}

	return nil
}

type bodyReader struct {
	r *http.Request
}

// ObjectSize implements thanos.ObjectSizer.
func (r bodyReader) ObjectSize() (int64, error) {
	return r.r.ContentLength, nil
}

// Read implements io.Reader.
func (r bodyReader) Read(b []byte) (int, error) {
	return r.r.Body.Read(b)
}
