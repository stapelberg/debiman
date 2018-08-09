package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"

	"github.com/stapelberg/debiman/internal/manpage"
	"github.com/stapelberg/debiman/internal/recode"
	"github.com/stapelberg/debiman/internal/write"

	"pault.ag/go/archive"
	"pault.ag/go/debian/control"
	"pault.ag/go/debian/deb"
	"pault.ag/go/debian/version"
)

// canSkip returns true if the package is present in the same (or a
// newer) version on disk already.
func canSkip(p pkgEntry, vPath string) bool {
	v, err := ioutil.ReadFile(vPath)
	if err != nil {
		return false
	}

	vCurrent, err := version.Parse(string(v))
	if err != nil {
		log.Printf("Warning: could not parse current package version from %q: %v", vPath, err)
		return false
	}

	return version.Compare(vCurrent, p.version) >= 0
}

type contentByBinarypkg []*contentEntry

func (p contentByBinarypkg) Len() int           { return len(p) }
func (p contentByBinarypkg) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p contentByBinarypkg) Less(i, j int) bool { return p[i].binarypkg < p[j].binarypkg }

// findClosestFile returns a manpage struct for name, if name exists in the same suite.
// TODO(stapelberg): resolve multiple matches: consider dependencies of src
func findClosestFile(logger *log.Logger, p pkgEntry, src, name string, contentByPath map[string][]*contentEntry) string {
	logger.Printf("findClosestFile(src=%q, name=%q)", src, name)
	c, ok := contentByPath[strings.TrimPrefix(name, "/usr/share/man/")]
	if !ok {
		return ""
	}

	// Ensure we only consider choices within the same suite.
	filtered := make([]*contentEntry, 0, len(c))
	for _, e := range c {
		if e.suite != p.suite {
			continue
		}
		filtered = append(filtered, e)
	}
	c = filtered

	// We still have more than one choice. In case the candidate is in
	// the same package as the source link, we take it.
	if len(c) > 1 {
		var last *contentEntry
		cnt := 0
		for _, e := range c {
			if e.binarypkg != p.binarypkg {
				continue
			}
			last = e
			if cnt++; cnt > 1 {
				break
			}
		}
		if cnt == 1 {
			c = []*contentEntry{last}
		}

		// We can’t make a 100% correct choice, but we can at least
		// make a deterministic choice. The user will see the
		// conflicting packages in the navigation panel to ultimately
		// resolve the situation, if necessary.
		sort.Sort(contentByBinarypkg(c))
	}

	if len(c) == 0 {
		return ""
	}

	m, err := manpage.FromManPath(strings.TrimPrefix(name, "/usr/share/man/"), &manpage.PkgMeta{
		Binarypkg: c[0].binarypkg,
		Suite:     c[0].suite,
	})
	logger.Printf("parsing %q as man: %v", name, err)
	if err == nil {
		return m.ServingPath() + ".gz"
	}

	return ""
}

func findFile(logger *log.Logger, src, name string, contentByPath map[string][]*contentEntry) (string, string, bool) {
	// TODO(later): why is "/"+ in front of src necessary?
	searchPath := []string{
		"/" + filepath.Dir(src), // “.”
		// To prefer manpages in the same language, add “..”, e.g.:
		// /usr/share/man/fr/man7/bash-builtins.7 references
		// man1/bash.1, which should be taken from
		// /usr/share/man/fr/man1/bash.1 instead of
		// /usr/share/man/man1/bash.1.
		"/" + filepath.Dir(src) + "/..",
		"/usr/share/man",
	}
	logger.Printf("searching reference so=%q", name)
	for _, search := range searchPath {
		var check string
		if filepath.IsAbs(name) {
			check = filepath.Clean(name)
		} else {
			check = filepath.Join(search, name)
		}
		// Some references include the .gz suffix, some don’t.
		if !strings.HasSuffix(check, ".gz") {
			check = check + ".gz"
		}

		c, ok := contentByPath[strings.TrimPrefix(check, "/usr/share/man/")]
		if !ok {
			log.Printf("%q does not exist", check)
			continue
		}

		m, err := manpage.FromManPath(strings.TrimPrefix(check, "/usr/share/man/"), &manpage.PkgMeta{
			Binarypkg: c[0].binarypkg,
			Suite:     c[0].suite,
		})
		logger.Printf("parsing %q as man: %v", check, err)
		if err == nil {
			return m.ServingPath() + ".gz", "", true
		}

		// TODO: we currently use the first manpage we find. this is non-deterministic, so sort.
		// TODO(later): try to resolve this reference intelligently, i.e. consider installability to narrow down the list of candidates. add a testcase with all cases that we have in all Debian suites currently
		return c[0].suite + "/" + c[0].binarypkg + "/aux" + check, check, true
	}
	return name, "", false
}

func soElim(logger *log.Logger, src string, r io.Reader, w io.Writer, contentByPath map[string][]*contentEntry) ([]string, error) {
	var refs []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, ".so ") {
			fmt.Fprintln(w, line)
			continue
		}
		so := strings.TrimSpace(line[len(".so "):])

		resolved, ref, ok := findFile(logger, src, so, contentByPath)
		if !ok {
			// Omitting .so lines which cannot be found is consistent
			// with what man(1) and other online man viewers do.
			logger.Printf("WARNING: could not find .so referenced file %q, omitting the .so line", so)
			continue
		}

		fmt.Fprintf(w, ".so %s\n", resolved)
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs, scanner.Err()
}

func writeManpage(logger *log.Logger, src, dest string, r io.Reader, m *manpage.Meta, contentByPath map[string][]*contentEntry) ([]string, error) {
	var refs []string
	content, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(content) {
		content, err = ioutil.ReadAll(recode.Reader(bytes.NewReader(content), m.Language))
		if err != nil {
			return nil, err
		}
	}
	err = write.Atomically(dest, true, func(w io.Writer) error {
		var err error
		refs, err = soElim(logger, src, bytes.NewReader(content), w, contentByPath)
		return err
	})
	return refs, err
}

func downloadPkg(ar *archive.Downloader, p pkgEntry, gv globalView) error {
	vPath := filepath.Join(*servingDir, p.suite, p.binarypkg, "VERSION")

	if !*forceReextract && canSkip(p, vPath) {
		return nil
	}

	logger := log.New(os.Stderr, p.suite+"/"+p.binarypkg+": ", log.LstdFlags)

	tmp, err := ar.TempFile(control.FileHash{
		Filename:  p.filename,
		Algorithm: "sha256",
		Hash:      fmt.Sprintf("%x", p.sha256),
	})
	if err != nil {
		return fmt.Errorf("archive download: %v", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if _, err := tmp.Seek(0, os.SEEK_SET); err != nil {
		return err
	}

	allRefs := make(map[string]bool)

	d, err := deb.Load(tmp, p.filename)
	if err != nil {
		return fmt.Errorf("loading %q: %v", p.filename, err)
	}
	for {
		header, err := d.Data.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if header.Typeflag != tar.TypeReg &&
			header.Typeflag != tar.TypeRegA &&
			header.Typeflag != tar.TypeSymlink &&
			header.Typeflag != tar.TypeLink {
			continue
		}
		if header.FileInfo().IsDir() {
			continue
		}
		if !strings.HasPrefix(header.Name, "./usr/share/man/") {
			continue
		}

		destdir := filepath.Join(*servingDir, p.suite, p.binarypkg)
		if err := os.MkdirAll(destdir, 0755); err != nil {
			return err
		}

		// TODO: return m?
		m, err := manpage.FromManPath(strings.TrimPrefix(header.Name, "./usr/share/man/"), &manpage.PkgMeta{
			Binarypkg: p.binarypkg,
			Suite:     p.suite,
		})

		if err != nil {
			logger.Printf("WARNING: file name %q (underneath /usr/share/man) cannot be parsed: %v", header.Name, err)
			continue
		}

		destPath := filepath.Join(*servingDir, m.ServingPath()+".gz")
		if header.Typeflag == tar.TypeLink {
			d, err := manpage.FromManPath(strings.TrimPrefix(header.Linkname, "./usr/share/man/"), &manpage.PkgMeta{
				Binarypkg: p.binarypkg,
				Suite:     p.suite,
			})
			if err != nil {
				logger.Printf("WARNING: hard link name %q (underneath /usr/share/man) cannot be parsed: %v", header.Linkname, err)
				continue
			}
			if err := os.Link(filepath.Join(*servingDir, d.ServingPath()+".gz"), m.ServingPath()+".gz"); err != nil {
				if os.IsExist(err) {
					continue
				}
				return err
			}
			continue
		}
		if header.Typeflag == tar.TypeSymlink {
			// filepath.Join calls filepath.Abs
			resolved := filepath.Join(filepath.Dir(strings.TrimPrefix(header.Name, ".")), header.Linkname)
			if !strings.HasSuffix(resolved, ".gz") {
				resolved = resolved + ".gz"
			}

			destsp := findClosestFile(logger, p, header.Name, resolved, gv.contentByPath)
			if destsp == "" {
				// Try to extract the resolved file as non-manpage
				// file. If the resolved file does not live in this
				// package, this will result in a dangling symlink.
				allRefs[resolved] = true
				destsp = filepath.Join(filepath.Dir(m.ServingPath()), "aux", resolved)
				logger.Printf("WARNING: possibly dangling symlink %q -> %q", header.Name, header.Linkname)
			}

			// TODO(stapelberg): add a unit test for this entire function
			// TODO(stapelberg): ganeti has an interesting twist: their manpages live outside of usr/share/man, and they only have symlinks. in this case, we should extract the file to aux/ and then mangle the symlink dest. problem: manpages actually are in a separate package (ganeti-2.15) and use an absolute symlink (/etc/ganeti/share), which is not shipped with the package.
			rel, err := filepath.Rel(filepath.Dir(m.ServingPath()), destsp)
			if err != nil {
				logger.Printf("WARNING: %v", err)
				continue
			}
			if err := os.Symlink(rel, destPath); err != nil {
				if os.IsExist(err) {
					continue
				}
				return err
			}
			if err := maybeSetLinkMtime(destPath, header.ModTime); err != nil {
				return err
			}

			continue
		}

		r := io.Reader(d.Data)
		var gzr *gzip.Reader
		if strings.HasSuffix(header.Name, ".gz") {
			gzr, err = gzip.NewReader(d.Data)
			if err != nil {
				return err
			}
			r = gzr
		}
		refs, err := writeManpage(logger, header.Name, destPath, r, m, gv.contentByPath)
		if err != nil {
			return err
		}
		if err := os.Chtimes(destPath, header.ModTime, header.ModTime); err != nil {
			return err
		}
		if gzr != nil {
			if err := gzr.Close(); err != nil {
				return err
			}
		}

		for _, r := range refs {
			allRefs[r] = true
		}
	}

	// Create all symlinks for slave alternatives.
	key := p.suite + "/" + p.binarypkg
	logger.Printf("creating %d links for binary package %q", len(gv.alternatives[key]), p.binarypkg)
	for _, link := range gv.alternatives[key] {
		if !strings.HasPrefix(link.from, "/usr/share/man/") {
			continue
		}

		m, err := manpage.FromManPath(strings.TrimPrefix(link.from, "/usr/share/man/"), &manpage.PkgMeta{
			Binarypkg: p.binarypkg,
			Suite:     p.suite,
		})
		if err != nil {
			logger.Printf("WARNING: file name %q (underneath /usr/share/man) cannot be parsed: %v", link.from, err)
			continue
		}

		resolved := link.to
		if !strings.HasSuffix(resolved, ".gz") {
			resolved = resolved + ".gz"
		}

		destsp := findClosestFile(logger, p, link.from, resolved, gv.contentByPath)
		if destsp == "" {
			// Try to extract the resolved file as non-manpage
			// file. If the resolved file does not live in this
			// package, this will result in a dangling symlink.
			allRefs[resolved] = true
			destsp = filepath.Join(filepath.Dir(m.ServingPath()), "aux", resolved)
			logger.Printf("WARNING: possibly dangling symlink %q -> %q, setting to %q", link.from, link.to, destsp)
		}

		// TODO(stapelberg): add a unit test for this entire function
		// TODO(stapelberg): ganeti has an interesting twist: their manpages live outside of usr/share/man, and they only have symlinks. in this case, we should extract the file to aux/ and then mangle the symlink dest. problem: manpages actually are in a separate package (ganeti-2.15) and use an absolute symlink (/etc/ganeti/share), which is not shipped with the package.
		rel, err := filepath.Rel(filepath.Dir(m.ServingPath()), destsp)
		if err != nil {
			logger.Printf("WARNING: %v", err)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(m.ServingPath()), 0755); err != nil {
			return err
		}

		if err := os.Symlink(rel, m.ServingPath()+".gz"); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
	}

	// Extract all non-manpage files which were referenced via .so
	// statements, if any.
	if len(allRefs) > 0 {
		if _, err := tmp.Seek(0, os.SEEK_SET); err != nil {
			return err
		}

		d, err = deb.Load(tmp, p.filename)
		if err != nil {
			return err
		}
		for {
			header, err := d.Data.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			if header.Typeflag != tar.TypeReg &&
				header.Typeflag != tar.TypeRegA &&
				header.Typeflag != tar.TypeSymlink {
				continue
			}

			if header.FileInfo().IsDir() {
				continue
			}

			if !allRefs[strings.TrimPrefix(header.Name, ".")] {
				continue
			}

			destPath := filepath.Join(*servingDir, p.suite, p.binarypkg, "aux", header.Name)
			logger.Printf("extracting referenced non-manpage file %q to %q", header.Name, destPath)
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			if err := write.Atomically(destPath, false, func(w io.Writer) error {
				_, err := io.Copy(w, d.Data)
				return err
			}); err != nil {
				return err
			}
		}
	}

	if err := ioutil.WriteFile(vPath, []byte(p.version.String()), 0644); err != nil {
		if os.IsNotExist(err) {
			// If the directory does not exist, we did not extract any
			// manpages. Since Contents files are not precise (they
			// might lag behind), this can happen occasionally.
			return nil
		}
		return fmt.Errorf("Writing version file %q: %v", vPath, err)
	}

	atomic.AddUint64(&gv.stats.PackagesExtracted, 1)

	return nil
}

func parallelDownload(ar *archive.Downloader, gv globalView) error {
	eg, ctx := errgroup.WithContext(context.Background())
	downloadChan := make(chan pkgEntry)
	// TODO: flag for parallelism level
	for i := 0; i < 10; i++ {
		eg.Go(func() error {
			for p := range downloadChan {
				if err := downloadPkg(ar, p, gv); err != nil {
					return fmt.Errorf("downloading %s/src:%s %v: %v", p.suite, p.source, p.version, err)
				}
			}
			return nil
		})
	}
	for _, p := range gv.pkgs {
		select {
		case downloadChan <- *p:
		case <-ctx.Done():
			break
		}
	}
	close(downloadChan)
	return eg.Wait()
}
