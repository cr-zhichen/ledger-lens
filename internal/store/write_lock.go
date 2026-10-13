package store

import (
	"os"

	"ledger-lens/internal/fault"
)

// Hold a separate file lock throughout read-modify-write-confirm, without
// holding a SQLite transaction over network I/O. Closing releases the lock.
func (s *Store) LockWrites() (*os.File, error) {
	file, err := os.OpenFile(s.path+".write.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, databaseError()
	}
	locked, err := tryWriteLock(file)
	if err != nil {
		file.Close()
		return nil, databaseError()
	}
	if !locked {
		file.Close()
		return nil, fault.New("WRITE_BUSY", "另一个账单写入正在进行，请等它结束后重试", 1)
	}
	return file, nil
}
