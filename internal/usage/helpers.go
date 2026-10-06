package usage

import (
	"path/filepath"
	"sort"
)

var filepathGlob = filepath.Glob

func sortTop(t []SessionCost) {
	sort.Slice(t, func(i, j int) bool { return t[i].Cost > t[j].Cost })
}
