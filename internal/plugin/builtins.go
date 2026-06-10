package plugin

import (
	gitdiff "github.com/hiveryn/git-diff/daemon"
	"github.com/hiveryn/tabplugin"
)

func init() {
	if err := tabplugin.Register("git-diff", gitdiff.New()); err != nil {
		panic("tabplugin register git-diff: " + err.Error())
	}
}
