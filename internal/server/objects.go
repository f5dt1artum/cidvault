package server

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/f5dt1artum/cidvault/internal/store"
)

const octetStream = "application/octet-stream"

type objectResponse struct {
	CID       string   `json:"cid"`
	Size      int64    `json:"size"`
	ChunkSize int      `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
	Created   bool     `json:"created"`
}

type manifestResponse struct {
	CID       string   `json:"cid"`
	Size      int64    `json:"size"`
	ChunkSize int      `json:"chunkSize"`
	Chunks    []string `json:"chunks"`
}

type errorBody struct {
	Error errorCode `json:"error"`
}

type errorCode struct {
	Code string `json:"code"`
}

func writeAPIError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorCode{Code: code}})
}

// validCID reports whether cid has the single supported shape:
// the "sha256:" scheme followed by exactly 64 lowercase hex digits.
func validCID(cid string) bool {
	const hexLen = 64
	if len(cid) != len(store.CIDPrefix)+hexLen {
		return false
	}
	if cid[:len(store.CIDPrefix)] != store.CIDPrefix {
		return false
	}
	for _, c := range cid[len(store.CIDPrefix):] {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func registerObjectRoutes(mux *http.ServeMux, st *store.Store) {
	mux.HandleFunc("/v1/objects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}

		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != octetStream {
			writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
			return
		}

		// Read at most one byte past the limit so an oversized body is
		// detected without buffering it fully. Nothing is stored on error,
		// so no partial object can remain.
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(store.MaxObjectSize)+1))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request")
			return
		}
		if int64(len(body)) > store.MaxObjectSize {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
			return
		}

		obj, created := st.Put(body)
		resp := objectResponse{
			CID:       obj.Root,
			Size:      obj.Size,
			ChunkSize: store.ChunkSize,
			Chunks:    obj.Chunks,
			Created:   created,
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/v1/objects/{cid}", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		cid := r.PathValue("cid")
		if !validCID(cid) {
			writeAPIError(w, http.StatusBadRequest, "invalid_cid")
			return
		}
		obj, chunks, ok := st.Get(cid)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "object_not_found")
			return
		}
		w.Header().Set("Content-Type", octetStream)
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = w.Write(chunk)
		}
	})

	mux.HandleFunc("/v1/objects/{cid}/manifest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		cid := r.PathValue("cid")
		if !validCID(cid) {
			writeAPIError(w, http.StatusBadRequest, "invalid_cid")
			return
		}
		obj, ok := st.Stat(cid)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "object_not_found")
			return
		}
		resp := manifestResponse{
			CID:       obj.Root,
			Size:      obj.Size,
			ChunkSize: store.ChunkSize,
			Chunks:    obj.Chunks,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
}
