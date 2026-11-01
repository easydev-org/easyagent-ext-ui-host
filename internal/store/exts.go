package store

import "os"

// Extensions lists the extension ids that have anything installed.
//
// A DIRECTORY WITHOUT A VERSION IS SKIPPED — an install that failed before its rename leaves a .tmp
// directory, and reporting it would offer a panel that cannot be opened.
func (s *Store) Extensions() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "" || e.Name()[0] == '.' {
			continue
		}
		versions, err := s.Installed(e.Name())
		if err != nil || len(versions) == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}
