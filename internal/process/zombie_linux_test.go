package process_test

import (
	"fmt"
	"os"
	"strings"
)

func zombiePOSIX(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(data), ')')
	return end >= 0 && strings.HasPrefix(string(data[end+1:]), " Z ")
}
