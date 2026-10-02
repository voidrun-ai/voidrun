package repository

import (
	"testing"

	"voidrun/model"
)

func TestShouldDeactivateStaleSystemImage(t *testing.T) {
	t.Parallel()

	valid := []model.Image{
		{Name: "code", Tag: "1.0.40"},
		{Name: "code", Tag: "1.0.38"},
	}

	if !shouldDeactivateStaleSystemImage("code", "1.0.30", valid) {
		t.Fatal("older tag of a listed name must be deactivated")
	}
	if shouldDeactivateStaleSystemImage("code", "1.0.40", valid) {
		t.Fatal("tag present in this seed must stay")
	}
	if shouldDeactivateStaleSystemImage("docker", "1.0.1", valid) {
		t.Fatal("name absent from this seed must stay, even when other images were listed")
	}
	if shouldDeactivateStaleSystemImage("code", "1.0.30", nil) {
		t.Fatal("empty seed must not deactivate anything")
	}
}
