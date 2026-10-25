//go:build !unix

package manager

import "io/fs"

func ownerOf(fs.FileInfo) (uid, gid int) { return -1, -1 }
