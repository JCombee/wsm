package api

import (
	"net/http"
	"net/url"

	"github.com/JCombee/wsm/internal/broker"
)

// maxBlockBody caps the body of PUT /v1/projects/{project}/blocks/{block}.
const maxBlockBody = 4 << 10

// Block is a project's ceiling for one block in API responses.
type Block struct {
	Block   string `json:"block"`
	Profile string `json:"profile"`
	// Tags are the allowed tags; the first one is the default.
	Tags []string `json:"tags"`
}

// SetBlockRequest is the body of PUT /v1/projects/{project}/blocks/{block}.
// An empty Profile means the block's default profile.
type SetBlockRequest struct {
	Profile string   `json:"profile,omitempty"`
	Tags    []string `json:"tags"`
}

// Service is a shared service instance in API responses. It never carries
// the instance's password, sealed or not.
type Service struct {
	Block   string `json:"block"`
	Tag     string `json:"tag"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	// State is "ready" or "down", checked when the list is requested.
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
}

func toBlock(c broker.Ceiling) Block {
	tags := c.Tags
	if tags == nil {
		tags = []string{}
	}
	return Block{Block: c.Block, Profile: c.Profile, Tags: tags}
}

func (s *Server) listBlocks(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Broker.Blocks(r.Context(), r.PathValue("project"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]Block, 0, len(cs))
	for _, c := range cs {
		out = append(out, toBlock(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// setBlock sets a ceiling. It waits until every instance it names is
// ready, which can take minutes the first time a tag is used.
func (s *Server) setBlock(w http.ResponseWriter, r *http.Request) {
	var req SetBlockRequest
	if err := decodeBody(r, maxBlockBody, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	c, err := s.Broker.SetBlock(r.Context(), r.PathValue("project"), r.PathValue("block"), req.Profile, req.Tags)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toBlock(c))
}

func (s *Server) removeBlock(w http.ResponseWriter, r *http.Request) {
	if err := s.Broker.RemoveBlock(r.Context(), r.PathValue("project"), r.PathValue("block")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	svcs, err := s.Broker.Services(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]Service, 0, len(svcs))
	for _, v := range svcs {
		out = append(out, Service{Block: v.Block, Tag: v.Tag, Address: v.Address, Port: v.Port,
			State: v.State, Version: v.Version})
	}
	writeJSON(w, http.StatusOK, out)
}

func blocksPath(project string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/blocks"
}
