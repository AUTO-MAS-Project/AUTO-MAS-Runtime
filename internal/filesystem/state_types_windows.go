package filesystem

import (
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// StateFiles 表示 Windows 受管状态目录能力。
type StateFiles struct {
	mu sync.RWMutex // 保护 closed、closeErr、pins、probePassed、classicUnlink、unlinkMode 与操作/关闭的句柄生命周期。

	layout              *config.Layout
	api                 pathAPI
	waitGate            WaitFunc
	fillNonce           func([]byte) error
	afterUnlinkVerified func(string)
	owner               *stateFileOwner
	pins                [2]pinnedObject
	probePassed         map[StateFileKind]bool
	classicUnlink       bool
	unlinkMode          string
	closed              bool
	closeErr            error
}
