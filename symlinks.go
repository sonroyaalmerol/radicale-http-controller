package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

var (
	watcher           *fsnotify.Watcher
	watcherMutex      sync.Mutex
	watcherSetup      sync.Once
	lastKnownConfig   *ControllerConfig
	lastKnownSymlinks []Symlink
)

func setupWatcher(cfg *ControllerConfig) error {
	var setupErr error
	watcherSetup.Do(func() {
		if cfg.RadicaleStoragePath == "" {
			log.Print("INFO: RADICALE_STORAGE_PATH not set. Filesystem watcher disabled.")
			setupErr = fmt.Errorf("storage path not configured") // Prevent watcher creation
			return
		}

		absStoragePath, err := filepath.Abs(cfg.RadicaleStoragePath)
		if err != nil {
			log.Printf("ERROR: Failed to get absolute path for watcher '%s': %v", cfg.RadicaleStoragePath, err)
			setupErr = err
			return
		}

		if _, err := os.Stat(absStoragePath); os.IsNotExist(err) {
			log.Printf("ERROR: Watcher cannot start, storage path '%s' does not exist.", absStoragePath)
			setupErr = err
			return
		}

		watcher, setupErr = fsnotify.NewWatcher()
		if setupErr != nil {
			log.Printf("ERROR: Failed to create filesystem watcher: %v", setupErr)
			return
		}

		go func() {
			defer watcher.Close() // Ensure watcher is closed on exit
			log.Printf("Filesystem watcher started for: %s", absStoragePath)
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok {
						log.Print("WARN: Filesystem watcher channel closed.")
						return
					}
					// We only care about new directories being created directly under storagePath
					if event.Op == fsnotify.Create {
						fileInfo, err := os.Stat(event.Name)
						// Check if it's a directory and directly under storagePath
						if err == nil && fileInfo.IsDir() && filepath.Dir(event.Name) == absStoragePath {
							newUserDir := filepath.Base(event.Name)
							log.Printf("Watcher detected new directory: %s", newUserDir)
							// Trigger immediate symlink application for this specific new user
							applySymlinksToUser(newUserDir, lastKnownSymlinks, absStoragePath)
						}
					}
				case err, ok := <-watcher.Errors:
					if !ok {
						log.Print("WARN: Filesystem watcher error channel closed.")
						return
					}
					log.Printf("ERROR: Filesystem watcher error: %v", err)
				}
			}
		}()

		setupErr = watcher.Add(absStoragePath)
		if setupErr != nil {
			log.Printf("ERROR: Failed to add path '%s' to watcher: %v", absStoragePath, setupErr)
			watcher.Close() // Close watcher if adding path failed
		}
	})
	return setupErr
}

// This function applies the known rules specifically to a single new user directory.
// It's called by the watcher.
func applySymlinksToUser(newUser string, rules []Symlink, storagePath string) {
	watcherMutex.Lock() // Basic lock to prevent concurrent filesystem ops if needed
	defer watcherMutex.Unlock()

	log.Printf("Applying symlink rules immediately to new user: %s", newUser)
	userPath := filepath.Join(storagePath, newUser)
	actionsTaken := 0

	for _, rule := range rules {
		cleanedSource := filepath.Clean(rule.Source)
		sourcePath := filepath.Join(storagePath, cleanedSource)
		linkName := filepath.Base(sourcePath)
		sourceOwner := strings.Split(cleanedSource, string(filepath.Separator))[0]

		if newUser == sourceOwner {
			continue // Skip creating symlink for the owner of the source
		}

		userPattern, err := regexp.Compile(rule.User)
		if err != nil {
			// Error already logged during full reconciliation, maybe skip logging here
			continue
		}

		if userPattern.MatchString(newUser) {
			linkPath := filepath.Join(userPath, linkName)

			// Check if source exists before creating link
			if _, err := os.Stat(sourcePath); err != nil {
				// Source might not exist yet, or permissions issue. Full reconciliation will handle it later.
				log.Printf("INFO: Source '%s' not found or inaccessible during immediate symlink for user '%s'. Skipping.", sourcePath, newUser)
				continue
			}

			// Check if something already exists at link path (unlikely for new user, but possible)
			if _, err := os.Lstat(linkPath); err == nil {
				log.Printf("WARN: Path '%s' already exists for new user '%s'. Skipping immediate symlink creation.", linkPath, newUser)
				continue
			}

			if err := os.Symlink(sourcePath, linkPath); err != nil {
				log.Printf("ERROR: Failed to create immediate symlink '%s' -> '%s': %v", linkPath, sourcePath, err)
			} else {
				log.Printf("CREATE (Immediate): Symlink '%s' -> '%s'", linkPath, sourcePath)
				actionsTaken++
			}
		}
	}
	if actionsTaken > 0 {
		log.Printf("Immediate symlink application for user '%s' finished. %d actions taken.", newUser, actionsTaken)
	} else {
		log.Printf("Immediate symlink application for user '%s' finished. No applicable links created.", newUser)
	}
}

func manageSymlinks(desiredSymlinks []Symlink, cfg *ControllerConfig) error {
	// Update global state for the watcher
	lastKnownConfig = cfg
	lastKnownSymlinks = desiredSymlinks

	// Setup watcher on first run (if not already setup or failed)
	if watcher == nil {
		_ = setupWatcher(cfg) // Attempt setup, ignore error here as main logic should proceed
	}

	storagePath := cfg.RadicaleStoragePath
	if storagePath == "" {
		log.Print(
			"INFO: RADICALE_STORAGE_PATH not set. Skipping full symlink reconciliation.",
		)
		// If watcher was previously running, close it? Or let it run hoping path gets configured?
		// For now, let's assume if path is gone, watcher should stop or not start.
		// The setupWatcher logic handles the initial non-existence.
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

	log.Printf("Starting full symlink reconciliation in '%s'", storagePath)

	watcherMutex.Lock() // Lock during full reconciliation
	defer watcherMutex.Unlock()

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
		sourceOwner := ""
		if strings.Contains(cleanedSource, string(filepath.Separator)) {
			sourceOwner = strings.Split(cleanedSource, string(filepath.Separator))[0]
		} else {
			// Handle cases where source might be directly under storage path? Unlikely for radicale.
			log.Printf("WARN: Source path '%s' does not seem to be within a user directory. Cannot determine owner.", cleanedSource)
			continue
		}

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
			// Logged during immediate creation attempt, maybe less verbose here?
			// log.Printf("WARN: Source path '%s' for symlink rule does not exist or cannot be accessed during full reconciliation. Skipping rule. Error: %v", sourcePath, err)
			continue
		}
		if !sourceInfo.IsDir() {
			// log.Printf("WARN: Source path '%s' is not a directory during full reconciliation. Skipping rule.", sourcePath)
			continue
		}

		for _, user := range targetUsers {
			if user == sourceOwner {
				continue
			}

			if userPattern.MatchString(user) {
				linkPath := filepath.Join(storagePath, user, linkName)
				if oldSource, exists := desiredState[linkPath]; exists {
					if oldSource != sourcePath {
						log.Printf(
							"WARN: Conflicting rules for symlink target '%s'. Rule for source '%s' overrides previous rule for source '%s'.",
							linkPath, sourcePath, oldSource,
						)
					}
				}
				desiredState[linkPath] = sourcePath
			}
		}
	}

	log.Printf(
		"Desired state calculated: %d symlinks across %d users.",
		len(desiredState), len(targetUsers),
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
				// log.Printf("WARN: Failed to lstat '%s'. Skipping entry. Error: %v", entryPath, err)
				continue
			}

			if fileInfo.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(entryPath)
				if err != nil {
					// log.Printf("WARN: Failed to read symlink '%s'. Skipping entry. Error: %v", entryPath, err)
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
			// Check source existence again right before linking
			if _, err := os.Stat(desiredSourcePath); err != nil {
				log.Printf("ERROR: Source path '%s' disappeared before creating link '%s'. Skipping.", desiredSourcePath, linkPath)
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
			// Check source existence again right before linking
			if _, err := os.Stat(desiredSourcePath); err != nil {
				log.Printf("ERROR: Source path '%s' disappeared before creating updated link '%s'. Skipping.", desiredSourcePath, linkPath)
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
		// If exists and actualSourcePath == desiredSourcePath, do nothing.
	}

	for linkPath, actualSourcePath := range actualState {
		if _, exists := desiredState[linkPath]; !exists {
			linkDirUser := filepath.Base(filepath.Dir(linkPath))
			sourceDirUser := ""
			if strings.HasPrefix(actualSourcePath, storagePath) {
				relPath, _ := filepath.Rel(storagePath, actualSourcePath)
				if strings.Contains(relPath, string(filepath.Separator)) {
					sourceDirUser = strings.Split(relPath, string(filepath.Separator))[0]
				}
			}

			if linkDirUser == sourceDirUser {
				log.Printf(
					"INFO: Skipping removal of link '%s' pointing to collection owned by the same user ('%s'). This might be manually created or from a previous configuration.",
					linkPath, linkDirUser,
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
		log.Printf("Full symlink reconciliation finished. %d actions taken.", actionsTaken)
	} else {
		log.Print("Full symlink reconciliation finished. No changes needed.")
	}

	return nil
}
