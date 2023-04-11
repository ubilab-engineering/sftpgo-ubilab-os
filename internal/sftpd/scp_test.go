//go:build !nos3
// +build !nos3

package sftpd

import (
	"fmt"
	"testing"
	"time"
)

// test insertPrefix
func TestInsertPrefix(t *testing.T) {
	var tests = []struct {
		name   string
		t      time.Time
		format string
		want   string
	}{ // look at the function I wrote in securelab
		{"ubi-sftpgo/biopyrenees/resultats_valides/R2200018.j1.pdf",
			time.Date(2023, 1, 1, 14, 30, 45, 0, time.Local),
			"2006",
			"ubi-sftpgo/biopyrenees/resultats_valides/2023/R2200018.j1.pdf"},
		{"ubi-sftpgo/biopyrenees/resultats_valides/R2200018.j1.pdf",
			time.Date(2023, 1, 1, 14, 30, 45, 0, time.Local),
			"2006/01/02",
			"ubi-sftpgo/biopyrenees/resultats_valides/2023/01/01/R2200018.j1.pdf"},
		{"ubi-sftpgo/biopyrenees/resultats_valides/R2200018.j1.pdf",
			time.Date(2023, 1, 1, 14, 30, 45, 0, time.Local),
			"2006/01/02/15/04/05",
			"ubi-sftpgo/biopyrenees/resultats_valides/2023/01/01/14/30/45/R2200018.j1.pdf"},
	}
	for _, tt := range tests {
		testname := fmt.Sprintf("%v,%v,%v", tt.name, tt.t, tt.format)
		t.Run(testname, func(t *testing.T) {
			if got := insertPrefix(tt.name, tt.t, tt.format); got != tt.want {
				t.Errorf("got = \n%v, want \n%v", got, tt.want)
			}
		})
	}
}
