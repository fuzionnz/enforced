package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/howeyc/fsnotify"
	logging "github.com/op/go-logging"
	goyaml "gopkg.in/yaml.v1"
)

type fileDescriptor struct {
	path *string
	info *os.FileInfo
}

var log *logging.Logger

func main() {
	// Load command line arguments
	configPath := flag.String("config", "", "The location of config yaml file")
	dryRun := flag.Bool("dry-run", false, "Don't actually do anything")
	verbose := flag.Bool("v", false, "Output verbose logging")
	veryVerbose := flag.Bool("vv", false, "Output highly verbose logging")
	syslog := flag.Bool("syslog", false, "Output logging to syslog")
	configCheck := flag.Bool("check", false, "Check config file and exit")
	flag.Parse()

	// Set up logging
	log = logging.MustGetLogger("enforced")

	// Set up syslog backend
	if *syslog {
		syslogBackend, err := logging.NewSyslogBackend("enforced")
		if err != nil {
			log.Criticalf("Failed to set up syslog backend: %s", err)
			os.Exit(1)
		}
		logging.SetBackend(syslogBackend)
	}

	// Set up logging
	switch {
	case *veryVerbose:
		logging.SetLevel(logging.DEBUG, "enforced")
		log.Debug("Very verbose logging enabled")
	case *verbose:
		logging.SetLevel(logging.INFO, "enforced")
		log.Info("Verbose logging enabled")
	default:
		logging.SetLevel(logging.ERROR, "enforced")
	}

	log.Info("Config path: %s", *configPath)
	if *dryRun {
		log.Info("Dry run enabled")
	}

	folderList, err := loadYAML(*configPath)
	if err != nil {
		log.Criticalf("Failed to load YAML config file: %s", err)
		os.Exit(1)
	}

	rootFolder, err := loadConfig(folderList, false)
	if err != nil {
		log.Criticalf("Failed to process config: %s", err)
		os.Exit(1)
	}
	log.Debug("%v", rootFolder)

	baseFolders := getBaseFolders(rootFolder)
	if len(baseFolders) == 0 {
		log.Critical("No configuration rules found.")
		os.Exit(1)
	}

	if *configCheck {
		return
	}

	// Remove base folders that don't exist or aren't folders
	tmp := make([]string, 0, len(baseFolders))
	for _, baseFolder := range baseFolders {
		if fi, err := os.Stat(baseFolder); err == nil && fi.IsDir() {
			tmp = append(tmp, baseFolder)
		} else {
			log.Errorf("Skipping inaccessible folder: %s", baseFolder)
		}
	}
	baseFolders = tmp

	ch := make(chan fileDescriptor, 1000)
	go updateFile(rootFolder, ch, *dryRun)

	// Start watching for file changes
	// While this means we will redundantly check any files we change
	// whilst we walk the full stack, it means we catch any files that change during the walk.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Criticalf("Error occurred creating new file watcher: %s", err)
		os.Exit(1)
	}
	// Fsnotify doesn't yet recursively watch. So we have to do this ourselves.
	for _, baseFolder := range baseFolders {
		err := recursivelyWatch(baseFolder, watcher)

		if err != nil {
			log.Criticalf("Error occurred adding folders to watcher: %s", err)
			os.Exit(1)
		}
	}

	// Let's make an initial walk across every single file and set it correctly.
	go func() {
		for _, baseFolder := range baseFolders {
			err := recursivelyUpdate(baseFolder, ch)

			if err != nil {
				// Exit if we can't walk full tree
				log.Criticalf("Error occurred walking files: %s", err)
				os.Exit(1)
			}
		}
	}()

	// Now let's start handling file changes until we're told to quit.
	for {
		select {
		case ev := <-watcher.Event:
			log.Debug(ev.String())
			if !(ev.IsCreate() || ev.IsAttrib()) {
				// We don't care. These events don't affect file ownership or permissions.
				continue
			}

			fileInfo, err := os.Stat(ev.Name)
			if err != nil {
				log.Errorf("Failed to stat changed file: %s", err)
				continue
			}
			mode := fileInfo.Mode()

			if ev.IsCreate() && mode.IsDir() {
				// Add new folders
				err := recursivelyWatch(ev.Name, watcher)
				if err != nil {
					log.Errorf("Failed to add new directory to watchlist: %s", err)
				} else {
					log.Debugf("Added new directory to watcher: %s", ev.Name)
				}

				// Send on children to be processed
				recursivelyUpdate(ev.Name, ch)
			} else {
				// Send on the file to be processed
				ch <- fileDescriptor{&ev.Name, &fileInfo}
			}

		case err := <-watcher.Error:
			log.Errorf("Watcher error: %s", err)
		}
	}
}

func loadYAML(path string) (folders []*folder, err error) {
	configFile, err := ioutil.ReadFile(path)
	if err != nil {
		return
	}

	type config struct {
		Folders []*folder
	}

	c := new(config)
	if err = goyaml.Unmarshal(configFile, c); err != nil {
		return
	}
	return c.Folders, nil
}

func loadConfig(folderList []*folder, ignoreSystemErrors bool) (rootFolder *folder, err error) {
	rootFolder = &folder{Path: string(os.PathSeparator)}

	for _, f := range folderList {
		// Path must always exist.
		if len(f.Path) == 0 {
			err = errors.New("all folders must have a path attribute")
			return
		}

		// Path must be absolute.
		if !filepath.IsAbs(f.Path) {
			err = errors.New("all folders must be absolute (ie. preceded by '/')")
			return
		}

		// Normalise filepaths
		f.Path = filepath.Clean(f.Path)

		// If user is set, grab system user id
		if f.User != "" {
			if f.Uid, err = getUserId(f.User); err != nil && !ignoreSystemErrors {
				// We failed to get the user id.
				err = fmt.Errorf("invalid user %s", f.User)
				return
			}
			err = nil // Reset error value if ignoreSystemErrors is true
		}

		// If the group is set, grab the system group id
		if f.Group != "" {
			if f.Gid, err = getGroupId(f.Group); err != nil && !ignoreSystemErrors {
				// We failed to get the group id
				err = fmt.Errorf("invalid group: %s", f.Group)
				return
			}
			err = nil // Reset error value if ignoreSystemErrors is true
		}

		// If file or dir perms are set, transform string to integer
		if f.FilePerms != "" {
			var fileMode uint64
			if fileMode, err = strconv.ParseUint(f.FilePerms, 8, 9); err == nil {
				f.FileMode = os.FileMode(fileMode)
			} else {
				err = fmt.Errorf("could not understand file perms: %s", f.FilePerms)
				return
			}
		}
		if f.DirPerms != "" {
			var dirMode uint64
			if dirMode, err = strconv.ParseUint(f.DirPerms, 8, 9); err == nil {
				f.DirMode = os.FileMode(dirMode)
			} else {
				err = fmt.Errorf("could not understand file perms: %s", f.DirPerms)
				return
			}
		}

		paths := strings.Split(f.Path, string(os.PathSeparator))
		currentFolder := rootFolder
		currentPath := []string{""}
		for i, p := range paths {
			// Ignore empty path components.
			if p == "" {
				continue
			}

			currentPath = append(currentPath, p)

			// Initialise children maps as we traverse the tree.
			if currentFolder.Children == nil {
				currentFolder.Children = make(map[string]*folder)
			}

			if i == len(paths)-1 {
				// Add folder configuration if we're at the last path component.
				newFolder := f
				currentFolder.Children[p] = newFolder
				currentFolder = newFolder
			} else if f, ok := currentFolder.Children[p]; ok {
				// Folder already exists and may contain config. Don't overwrite.
				currentFolder = f
			} else {
				// No child exists. Create empty placeholder folder configuration.
				newFolder := &folder{Path: strings.Join(currentPath, string(os.PathSeparator))}
				currentFolder.Children[p] = newFolder
				currentFolder = newFolder
			}
		}
	}
	return
}

func getBaseFolders(f *folder) []string {
	if f.hasConfig() {
		return []string{f.Path}
	}

	baseFolders := []string{}
	for _, f := range f.Children {
		baseFolders = append(baseFolders, getBaseFolders(f)...)
	}

	return baseFolders
}

func getConfig(paths []string, currentFolder *folder, config *folder) {
	config.mergeConfig(currentFolder)

	// Check if we've reached our final destination
	if len(paths) == 0 {
		return
	}

	// Attempt to find next child node
	if nextFolder, ok := currentFolder.Children[paths[0]]; ok {
		// Child folder config exists. Recurse.
		getConfig(paths[1:], nextFolder, config)
		return
	}

	// Otherwise this is as far as we can go. We have our config.
	return
}

func recursivelyWatch(folder string, watcher *fsnotify.Watcher) (err error) {
	err = filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			err = watcher.Watch(path)
		}
		return err
	})

	return
}

func recursivelyUpdate(folder string, ch chan fileDescriptor) (err error) {
	err = filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		ch <- fileDescriptor{&path, &info}
		return nil
	})
	return
}

func updateFile(rootFolder *folder, ch chan fileDescriptor, dryRun bool) {
	for f := range ch {
		log.Debugf("Processing file: %s", *f.path)

		// Extract file/folder information
		sys := (*f.info).Sys()
		if sys == nil {
			log.Errorf("Skipping file: sys interface is nil for %s", *f.path)
			return
		}
		uid := int(sys.(*syscall.Stat_t).Uid)
		gid := int(sys.(*syscall.Stat_t).Gid)
		mode := (*f.info).Mode()
		perms := mode.Perm()
		isDir := mode.IsDir()
		isRegular := mode.IsRegular()

		// We only know how to handle regular files and directories.
		// We ignore symlinks as on Linux this changes the ownership of the linked file instead.
		if !(isDir || isRegular) {
			log.Infof("Skipping file: neither regular file or directory %s", *f.path)
			continue
		}

		// Explode path into compononents, and remove first component if it is empty.
		paths := strings.Split(*f.path, string(os.PathSeparator))
		if len(paths) > 0 && paths[0] == "" {
			paths = paths[1:]
		}

		// Get config for this file/folder
		c := &folder{}
		getConfig(paths, rootFolder, c)

		// If user/group is empty, then we want the file's owner/group to remain unchanged.
		if c.User == "" {
			c.Uid = uid
		}
		if c.Group == "" {
			c.Gid = gid
		}

		// Set permissions for directories.
		if isDir && c.DirMode != 0 && perms != c.DirMode {
			log.Infof("%s Changing permissions to %s\n", *f.path, c.DirMode)
			if !dryRun {
				if err := os.Chmod(*f.path, c.DirMode); err != nil {
					log.Errorf("%s", err)
				}
			}
		}
		// Set permissions for files.
		if isRegular && c.FileMode != 0 && perms != c.FileMode {
			log.Infof("%s Changing permissions to %s\n", *f.path, c.FileMode)
			if !dryRun {
				if err := os.Chmod(*f.path, c.FileMode); err != nil {
					log.Errorf("%s", err)
				}
			}
		}
		// Set ownership for files and directories.
		if uid != c.Uid || gid != c.Gid {
			log.Info("%s Changing ownership to %s (%d) / %s (%d)\n", *f.path, c.User, c.Uid, c.Group, c.Gid)
			if !dryRun {
				if err := os.Chown(*f.path, c.Uid, c.Gid); err != nil {
					log.Errorf("%s", err)
				}
			}
		}
	}
}
