package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/influxdata/influxdb"
	pcontext "github.com/influxdata/influxdb/context"
	"github.com/julienschmidt/httprouter"
	"go.uber.org/zap"
)

// DocumentBackend is all services and associated parameters required to construct
// the DocumentHandler.
type DocumentBackend struct {
	Logger *zap.Logger

	DocumentService influxdb.DocumentService
}

// NewDocumentBackend returns a new instance of DocumentBackend.
func NewDocumentBackend(b *APIBackend) *DocumentBackend {
	return &DocumentBackend{
		Logger:          b.Logger.With(zap.String("handler", "document")),
		DocumentService: b.DocumentService,
	}
}

// DocumentHandler represents an HTTP API handler for documents.
type DocumentHandler struct {
	*httprouter.Router

	Logger *zap.Logger

	DocumentService influxdb.DocumentService
}

const (
	documentsPath = "/api/v2/documents/:ns"
	documentPath  = "/api/v2/documents/:ns/:id"
)

// TODO(desa): this should probably take a namespace
// NewDocumentHandler returns a new instance of DocumentHandler.
func NewDocumentHandler(b *DocumentBackend) *DocumentHandler {
	h := &DocumentHandler{
		Router: NewRouter(),
		Logger: b.Logger,

		DocumentService: b.DocumentService,
	}

	h.HandlerFunc("POST", documentsPath, h.handlePostDocument)
	h.HandlerFunc("GET", documentsPath, h.handleGetDocuments)
	h.HandlerFunc("GET", documentPath, h.handleGetDocument)
	h.HandlerFunc("PUT", documentPath, h.handlePutDocument)
	h.HandlerFunc("DELETE", documentPath, h.handleDeleteDocument)

	return h
}

type documentResponse struct {
	Links map[string]string `json:"links"`
	*influxdb.Document
}

func newDocumentResponse(ns string, d *influxdb.Document) *documentResponse {
	if d.Labels == nil {
		d.Labels = []*influxdb.Label{}
	}
	return &documentResponse{
		Links: map[string]string{
			"self": fmt.Sprintf("/api/v2/documents/%s/%s", ns, d.ID),
		},
		Document: d,
	}
}

type documentsResponse struct {
	Documents []*documentResponse `json:"documents"`
}

func newDocumentsResponse(ns string, docs []*influxdb.Document) *documentsResponse {
	ds := make([]*documentResponse, 0, len(docs))
	for _, doc := range docs {
		ds = append(ds, newDocumentResponse(ns, doc))
	}

	return &documentsResponse{
		Documents: ds,
	}
}

// handlePostDocument is the HTTP handler for the POST /api/v2/documents/:ns route.
func (h *DocumentHandler) handlePostDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := decodePostDocumentRequest(ctx, r)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	s, err := h.DocumentService.FindDocumentStore(ctx, req.Namespace)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	a, err := pcontext.GetAuthorizer(ctx)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	opts := []influxdb.DocumentOptions{}
	if req.OrgID.Valid() {
		opts = append(opts, influxdb.AuthorizedWithOrgID(a, req.OrgID))
	} else {
		opts = append(opts, influxdb.AuthorizedWithOrg(a, req.Org))
	}
	for _, label := range req.Labels {
		// TODO(desa): make these AuthorizedWithLabel eventually
		opts = append(opts, influxdb.WithLabel(label))
	}

	if err := s.CreateDocument(ctx, req.Document, opts...); err != nil {
		EncodeError(ctx, err, w)
		return
	}

	if err := encodeResponse(ctx, w, http.StatusCreated, newDocumentResponse(req.Namespace, req.Document)); err != nil {
		logEncodingError(h.Logger, r, err)
		return
	}
}

type postDocumentRequest struct {
	*influxdb.Document
	Namespace string      `json:"-"`
	Org       string      `json:"org"`
	OrgID     influxdb.ID `json:"orgID,omitempty"`
	Labels    []string    `json:"labels"` // TODO(desa): should this be IDs or strings?
}

func decodePostDocumentRequest(ctx context.Context, r *http.Request) (*postDocumentRequest, error) {
	req := &postDocumentRequest{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		return nil, err
	}

	if req.Document == nil {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "missing document body",
		}
	}

	params := httprouter.ParamsFromContext(ctx)
	req.Namespace = params.ByName("ns")
	if req.Namespace == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing namespace",
		}
	}

	return req, nil
}

// handleGetDocuments is the HTTP handler for the GET /api/v2/documents/:ns route.
func (h *DocumentHandler) handleGetDocuments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := decodeGetDocumentsRequest(ctx, r)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	s, err := h.DocumentService.FindDocumentStore(ctx, req.Namespace)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	a, err := pcontext.GetAuthorizer(ctx)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	var opt func(influxdb.DocumentIndex, influxdb.DocumentDecorator) ([]influxdb.ID, error)

	if req.Org != "" && req.OrgID != nil {
		EncodeError(ctx, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "Please provide either org or orgID, not both",
		}, w)
		return
	} else if req.OrgID != nil && req.OrgID.Valid() {
		opt = influxdb.AuthorizedWhereOrgID(a, *req.OrgID)
	} else if req.Org != "" {
		opt = influxdb.AuthorizedWhereOrg(a, req.Org)
	} else {
		EncodeError(ctx, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "Please provide either org or orgID",
		}, w)
		return
	}

	ds, err := s.FindDocuments(ctx, opt, influxdb.IncludeLabels)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	if err := encodeResponse(ctx, w, http.StatusOK, newDocumentsResponse(req.Namespace, ds)); err != nil {
		logEncodingError(h.Logger, r, err)
		return
	}
}

type getDocumentsRequest struct {
	Namespace string
	Org       string
	OrgID     *influxdb.ID
}

func decodeGetDocumentsRequest(ctx context.Context, r *http.Request) (*getDocumentsRequest, error) {
	params := httprouter.ParamsFromContext(ctx)
	ns := params.ByName("ns")
	if ns == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing namespace",
		}
	}

	qp := r.URL.Query()
	var oid *influxdb.ID
	var err error

	if oidStr := qp.Get("orgID"); oidStr != "" {
		oid, err = influxdb.IDFromString(oidStr)
		if err != nil {
			return nil, &influxdb.Error{
				Code: influxdb.EInvalid,
				Msg:  "Invalid orgID",
			}
		}
	}
	return &getDocumentsRequest{
		Namespace: ns,
		Org:       qp.Get("org"),
		OrgID:     oid,
	}, nil
}

// handleGetDocument is the HTTP handler for the GET /api/v2/documents/:ns/:id route.
func (h *DocumentHandler) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := decodeGetDocumentRequest(ctx, r)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	s, err := h.DocumentService.FindDocumentStore(ctx, req.Namespace)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	a, err := pcontext.GetAuthorizer(ctx)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	ds, err := s.FindDocuments(ctx, influxdb.AuthorizedWhereID(a, req.ID), influxdb.IncludeContent, influxdb.IncludeLabels)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	if len(ds) != 1 {
		err := &influxdb.Error{
			Code: influxdb.EInternal,
			Msg:  fmt.Sprintf("found more than one document with id %s; please report this error", req.ID),
		}
		EncodeError(ctx, err, w)
		return
	}

	d := ds[0]

	if err := encodeResponse(ctx, w, http.StatusOK, newDocumentResponse(req.Namespace, d)); err != nil {
		logEncodingError(h.Logger, r, err)
		return
	}
}

type getDocumentRequest struct {
	Namespace string
	ID        influxdb.ID
}

func decodeGetDocumentRequest(ctx context.Context, r *http.Request) (*getDocumentRequest, error) {
	params := httprouter.ParamsFromContext(ctx)
	ns := params.ByName("ns")
	if ns == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing namespace",
		}
	}

	i := params.ByName("id")
	if i == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing id",
		}
	}

	var id influxdb.ID
	if err := id.DecodeFromString(i); err != nil {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "bad id in url",
		}
	}

	return &getDocumentRequest{
		Namespace: ns,
		ID:        id,
	}, nil
}

// handleDeleteDocument is the HTTP handler for the DELETE /api/v2/documents/:ns/:id route.
func (h *DocumentHandler) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := decodeDeleteDocumentRequest(ctx, r)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	s, err := h.DocumentService.FindDocumentStore(ctx, req.Namespace)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	a, err := pcontext.GetAuthorizer(ctx)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	if err := s.DeleteDocuments(ctx, influxdb.AuthorizedWhereID(a, req.ID)); err != nil {
		EncodeError(ctx, err, w)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type deleteDocumentRequest struct {
	Namespace string
	ID        influxdb.ID
}

func decodeDeleteDocumentRequest(ctx context.Context, r *http.Request) (*deleteDocumentRequest, error) {
	params := httprouter.ParamsFromContext(ctx)
	ns := params.ByName("ns")
	if ns == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing namespace",
		}
	}

	i := params.ByName("id")
	if i == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing id",
		}
	}

	var id influxdb.ID
	if err := id.DecodeFromString(i); err != nil {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "bad id in url",
		}
	}

	return &deleteDocumentRequest{
		Namespace: ns,
		ID:        id,
	}, nil
}

// handlePutDocument is the HTTP handler for the PUT /api/v2/documents/:ns/:id route.
func (h *DocumentHandler) handlePutDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	req, err := decodePutDocumentRequest(ctx, r)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	s, err := h.DocumentService.FindDocumentStore(ctx, req.Namespace)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	a, err := pcontext.GetAuthorizer(ctx)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	if err := s.UpdateDocument(ctx, req.Document, influxdb.Authorized(a)); err != nil {
		EncodeError(ctx, err, w)
		return
	}

	ds, err := s.FindDocuments(ctx, influxdb.WhereID(req.Document.ID), influxdb.IncludeContent)
	if err != nil {
		EncodeError(ctx, err, w)
		return
	}

	if len(ds) != 1 {
		err := &influxdb.Error{
			Code: influxdb.EInternal,
			Msg:  fmt.Sprintf("found more than one document with id %s; please report this error", req.ID),
		}
		EncodeError(ctx, err, w)
		return
	}

	d := ds[0]

	if err := encodeResponse(ctx, w, http.StatusOK, newDocumentResponse(req.Namespace, d)); err != nil {
		logEncodingError(h.Logger, r, err)
		return
	}
}

type putDocumentRequest struct {
	*influxdb.Document
	Namespace string `json:"-"`
}

func decodePutDocumentRequest(ctx context.Context, r *http.Request) (*putDocumentRequest, error) {
	req := &putDocumentRequest{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		return nil, err
	}

	params := httprouter.ParamsFromContext(ctx)
	i := params.ByName("id")
	if i == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing id",
		}
	}

	if err := req.ID.DecodeFromString(i); err != nil {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Err:  err,
		}
	}

	req.Namespace = params.ByName("ns")
	if req.Namespace == "" {
		return nil, &influxdb.Error{
			Code: influxdb.EInvalid,
			Msg:  "url missing namespace",
		}
	}

	return req, nil
}
