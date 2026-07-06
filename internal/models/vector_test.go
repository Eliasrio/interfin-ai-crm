package models

import (
	"reflect"
	"testing"
)

func TestVector_ValueScanRoundtrip(t *testing.T) {
	src := Vector{0.5, -1, 0.25, 3}
	val, err := src.Value()
	if err != nil {
		t.Fatal(err)
	}
	if val != "[0.5,-1,0.25,3]" {
		t.Fatalf("Value() = %q, ожидали литерал pgvector", val)
	}

	var got Vector
	if err := got.Scan(val); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, src) {
		t.Fatalf("roundtrip: %v != %v", got, src)
	}
}

func TestVector_ScanRejectsGarbage(t *testing.T) {
	var v Vector
	if err := v.Scan("не вектор"); err == nil {
		t.Fatal("ожидали ошибку на невекторном литерале")
	}
}

func TestVector_EmptyValueIsNull(t *testing.T) {
	val, err := Vector(nil).Value()
	if err != nil {
		t.Fatal(err)
	}
	if val != nil {
		t.Fatalf("пустой вектор обязан кодироваться NULL, получили %v", val)
	}
}
