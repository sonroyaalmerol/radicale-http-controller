package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func manageSymlinks(desiredSymlinks []Symlink, storagePath string) error {
	if storagePath == "" {
		log.Print(
			"INFO: RADICALE_STORAGE_PATH not set. Skipping symlink management.",
		)
		return nil
	}

	storagePath, err := filepath.Abs(storagePath)
	if err != nil {
		return fmt.Errorf(
			"failed to get absolute path for storage '%s': %w",
			storagePath,
			err,
		)
	}

	storageInfo, err := os.Stat(storagePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(
				"radicale storage path '%s' does not exist",
				storagePath,
			)
		}
		return fmt.Errorf(
			"failed to stat storage path '%s': %w",
			storagePath,
			err,
		)
	}
	if !storageInfo.IsDir() {
		return fmt.Errorf(
			"radicale storage path '%s' is not a directory",
			storagePath,
		)
	}

	log.Printf("Starting symlink management in '%s'", storagePath)

	desiredState := make(map[string]string)
	actualState := make(map[string]string)

	userDirs, err := os.ReadDir(storagePath)
	if err != nil {
		return fmt.Errorf(
			"failed to read storage directory '%s': %w",
			storagePath,
			err,
		)
	}

	targetUsers := []string{}
	for _, entry := range userDirs {
		if entry.IsDir() {
			targetUsers = append(targetUsers, entry.Name())
		}
	}

	for _, rule := range desiredSymlinks {
		cleanedSource := filepath.Clean(rule.Source)
		sourcePath := filepath.Join(storagePath, cleanedSource)
		linkName := filepath.Base(sourcePath)
		sourceOwner := strings.Split(cleanedSource, string(filepath.Separator))[0]

		userPattern, err := regexp.Compile(rule.User)
		if err != nil {
			log.Printf(
				"WARN: Invalid regex pattern '%s' for source '%s'. Skipping rule. Error: %v",
				rule.User,
				rule.Source,
				err,
			)
			continue
		}

		sourceInfo, err := os.Stat(sourcePath)
		if err != nil {
			log.Printf(
				"WARN: Source path '%s' for symlink rule does not exist or cannot be accessed. Skipping rule. Error: %v",
				sourcePath,
				err,
			)
			continue
		}
		if !sourceInfo.IsDir() {
			log.Printf(
				"WARN: Source path '%s' is not a directory. Skipping rule.",
				sourcePath,
			)
			continue
		}

		for _, user := range targetUsers {
			if user == sourceOwner {
				continue // Skip creating symlink for the owner of the source
			}

			if userPattern.MatchString(user) {
				linkPath := filepath.Join(storagePath, user, linkName)
				if _, exists := desiredState[linkPath]; exists {
					log.Printf(
						"WARN: Duplicate desired symlink target '%s'. Check configuration rules.",
						linkPath,
					)
				}
				desiredState[linkPath] = sourcePath
			}
		}
	}

	log.Printf(
		"Desired state calculated: %d symlinks across matched non-owner users.",
		len(desiredState),
	)

	for _, user := range targetUsers {
		userPath := filepath.Join(storagePath, user)
		entries, err := os.ReadDir(userPath)
		if err != nil {
			log.Printf(
				"WARN: Failed to read user directory '%s'. Skipping scan for this user. Error: %v",
				userPath,
				err,
			)
			continue
		}

		for _, entry := range entries {
			entryPath := filepath.Join(userPath, entry.Name())
			fileInfo, err := os.Lstat(entryPath)
			if err != nil {
				log.Printf(
					"WARN: Failed to lstat '%s'. Skipping entry. Error: %v",
					entryPath,
					err,
				)
				continue
			}

			if fileInfo.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(entryPath)
				if err != nil {
					log.Printf(
						"WARN: Failed to read symlink '%s'. Skipping entry. Error: %v",
						entryPath,
						err,
					)
					continue
				}

				resolvedTarget := target
				if !filepath.IsAbs(target) {
					resolvedTarget = filepath.Join(filepath.Dir(entryPath), target)
				}
				resolvedTarget = filepath.Clean(resolvedTarget)

				actualState[entryPath] = resolvedTarget
			}
		}
	}

	log.Printf("Actual state scanned: %d symlinks found.", len(actualState))

	actionsTaken := 0

	for linkPath, desiredSourcePath := range desiredState {
		actualSourcePath, exists := actualState[linkPath]

		if !exists {
			targetDir := filepath.Dir(linkPath)
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				log.Printf(
					"ERROR: Failed to create target directory '%s' for link '%s': %v",
					targetDir,
					linkPath,
					err,
				)
				continue
			}
			if err := os.Symlink(desiredSourcePath, linkPath); err != nil {
				log.Printf(
					"ERROR: Failed to create symlink '%s' -> '%s': %v",
					linkPath,
					desiredSourcePath,
					err,
				)
			} else {
				log.Printf("CREATE: Symlink '%s' -> '%s'", linkPath, desiredSourcePath)
				actionsTaken++
			}
		} else if actualSourcePath != desiredSourcePath {
			if err := os.Remove(linkPath); err != nil {
				log.Printf(
					"ERROR: Failed to remove incorrect symlink '%s': %v",
					linkPath,
					err,
				)
				continue
			}
			if err := os.Symlink(desiredSourcePath, linkPath); err != nil {
				log.Printf(
					"ERROR: Failed to create updated symlink '%s' -> '%s': %v",
					linkPath,
					desiredSourcePath,
					err,
				)
			} else {
				log.Printf(
					"UPDATE: Symlink '%s' -> '%s' (was '%s')",
					linkPath,
					desiredSourcePath,
					actualSourcePath,
				)
				actionsTaken++
			}
		}
	}

	for linkPath, actualSourcePath := range actualState {
		if _, exists := desiredState[linkPath]; !exists {
			// Before removing, double-check it's not a link pointing to itself
			// which might happen if the owner check failed previously or config changed.
			linkDir := filepath.Dir(linkPath)
			linkBase := filepath.Base(linkPath)
			potentialSelfSource := filepath.Join(linkDir, linkBase)
			if actualSourcePath == potentialSelfSource {
				log.Printf(
					"INFO: Skipping removal of self-referential link '%s'. This might indicate a previous state or manual change.",
					linkPath,
				)
				continue
			}

			if err := os.Remove(linkPath); err != nil {
				log.Printf(
					"ERROR: Failed to remove extraneous symlink '%s' -> '%s': %v",
					linkPath,
					actualSourcePath,
					err,
				)
			} else {
				log.Printf(
					"REMOVE: Extraneous symlink '%s' -> '%s'",
					linkPath,
					actualSourcePath,
				)
				actionsTaken++
			}
		}
	}

	if actionsTaken > 0 {
		log.Printf("Symlink management finished. %d actions taken.", actionsTaken)
	} else {
		log.Print("Symlink management finished. No changes needed.")
	}

	return nil
}
