package server

import (
	"strings"
	"testing"

	"voidrun/config"

	"github.com/gin-gonic/gin"
)

func TestSetupRoutes_RejectsSecondCall(t *testing.T) {
	s := &Server{router: gin.New()}
	if err := s.SetupRoutes(nil); err == nil {
		t.Fatal("expected error on second SetupRoutes")
	}
}

func TestRun_RequiresRoutes(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	err := s.Run()
	if err == nil {
		t.Fatal("expected error when Run is called before SetupRoutes")
	}
	if !strings.Contains(err.Error(), "SetupRoutes") {
		t.Fatalf("got %q, want mention of SetupRoutes", err)
	}
}
