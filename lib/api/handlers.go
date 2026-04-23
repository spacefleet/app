package api

import (
	"context"
	"fmt"
)

type Server struct{}

func NewServer() *Server {
	return &Server{}
}

var _ StrictServerInterface = (*Server)(nil)

func (s *Server) GetHealth(_ context.Context, _ GetHealthRequestObject) (GetHealthResponseObject, error) {
	return GetHealth200JSONResponse{Status: Ok}, nil
}

func (s *Server) GetPing(_ context.Context, req GetPingRequestObject) (GetPingResponseObject, error) {
	name := "world"
	if req.Params.Name != nil && *req.Params.Name != "" {
		name = *req.Params.Name
	}
	return GetPing200JSONResponse{Message: fmt.Sprintf("hello, %s", name)}, nil
}
