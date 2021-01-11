package helm

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/artifacthub/hub/internal/hub"
	"github.com/artifacthub/hub/internal/license"
	"github.com/artifacthub/hub/internal/repo"
	"github.com/artifacthub/hub/internal/tracker"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/deislabs/oras/pkg/content"
	ctxo "github.com/deislabs/oras/pkg/context"
	"github.com/deislabs/oras/pkg/oras"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/vincent-petithory/dataurl"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

const (
	changesAnnotation              = "artifacthub.io/changes"
	crdsAnnotation                 = "artifacthub.io/crds"
	crdsExamplesAnnotation         = "artifacthub.io/crdsExamples"
	imagesAnnotation               = "artifacthub.io/images"
	licenseAnnotation              = "artifacthub.io/license"
	linksAnnotation                = "artifacthub.io/links"
	maintainersAnnotation          = "artifacthub.io/maintainers"
	operatorAnnotation             = "artifacthub.io/operator"
	operatorCapabilitiesAnnotation = "artifacthub.io/operatorCapabilities"
	prereleaseAnnotation           = "artifacthub.io/prerelease"
	securityUpdatesAnnotation      = "artifacthub.io/containsSecurityUpdates"

	helmChartConfigMediaType       = "application/vnd.cncf.helm.config.v1+json"
	helmChartContentLayerMediaType = "application/tar+gzip"
)

// Worker is in charge of handling Helm packages register and unregister jobs
// generated by the tracker.
type Worker struct {
	svc    *tracker.Services
	r      *hub.Repository
	logger zerolog.Logger
}

// NewWorker creates a new worker instance.
func NewWorker(
	svc *tracker.Services,
	r *hub.Repository,
) *Worker {
	return &Worker{
		svc:    svc,
		r:      r,
		logger: log.With().Str("repo", r.Name).Str("kind", hub.GetKindName(r.Kind)).Logger(),
	}
}

// Run instructs the worker to start handling jobs. It will keep running until
// the jobs queue is empty or the context is done.
func (w *Worker) Run(wg *sync.WaitGroup, queue chan *Job) {
	defer wg.Done()
	for {
		select {
		case j, ok := <-queue:
			if !ok {
				return
			}
			switch j.Kind {
			case Register:
				w.handleRegisterJob(j)
			case Unregister:
				w.handleUnregisterJob(j)
			}
		case <-w.svc.Ctx.Done():
			return
		}
	}
}

// handleRegisterJob handles the provided Helm package registration job. This
// involves downloading the chart archive, extracting its contents and register
// the corresponding package.
func (w *Worker) handleRegisterJob(j *Job) {
	md := j.ChartVersion.Metadata

	defer func() {
		if r := recover(); r != nil {
			w.logger.Error().
				Str("package", md.Name).
				Str("version", md.Version).
				Bytes("stacktrace", debug.Stack()).
				Interface("recover", r).
				Msg("handleRegisterJob panic")
		}
	}()

	// Prepare chart archive url
	chartURL, err := url.Parse(j.ChartVersion.URLs[0])
	if err != nil {
		w.warn(md, fmt.Errorf("invalid chart url %s: %w", j.ChartVersion.URLs[0], err))
		return
	}
	if !chartURL.IsAbs() {
		repoURL, _ := url.Parse(w.r.URL)
		chartURL.Scheme = repoURL.Scheme
		chartURL.Host = repoURL.Host
		if !strings.HasPrefix(chartURL.Path, "/") {
			chartURL.Path = path.Join(repoURL.Path, chartURL.Path)
		}
	}

	// Load chart from remote archive
	chart, err := w.loadChart(chartURL)
	if err != nil {
		w.warn(md, fmt.Errorf("error loading chart (%s): %w", chartURL.String(), err))
		return
	}
	indexName, indexVersion := md.Name, md.Version
	md = chart.Metadata
	if md.Name != indexName || md.Version != indexVersion {
		w.warn(md, fmt.Errorf("name and version in index (%s:%s) do not match chart content", indexName, indexVersion))
		return
	}

	// Store logo when available if requested
	var logoURL, logoImageID string
	if j.StoreLogo && md.Icon != "" {
		logoURL = md.Icon
		data, err := w.getImage(md.Icon)
		if err != nil {
			w.warn(md, fmt.Errorf("error getting image %s: %w", md.Icon, err))
		} else {
			logoImageID, err = w.svc.Is.SaveImage(w.svc.Ctx, data)
			if err != nil && !errors.Is(err, image.ErrFormat) {
				w.warn(md, fmt.Errorf("error saving image %s: %w", md.Icon, err))
			}
		}
	}

	// Prepare package to be registered
	p := &hub.Package{
		Name:         md.Name,
		LogoURL:      logoURL,
		LogoImageID:  logoImageID,
		Description:  md.Description,
		Keywords:     md.Keywords,
		HomeURL:      md.Home,
		Version:      md.Version,
		AppVersion:   md.AppVersion,
		Digest:       j.ChartVersion.Digest,
		Deprecated:   md.Deprecated,
		ContentURL:   chartURL.String(),
		ValuesSchema: chart.Schema,
		Repository:   w.r,
	}
	if !j.ChartVersion.Created.IsZero() {
		p.CreatedAt = j.ChartVersion.Created.Unix()
	}
	readme := getFile(chart, "README.md")
	if readme != nil {
		p.Readme = string(readme.Data)
	}
	licenseFile := getFile(chart, "LICENSE")
	if licenseFile != nil {
		p.License = license.Detect(licenseFile.Data)
	}
	if repo.SchemeIsHTTP(chartURL) {
		hasProvenanceFile, err := w.chartVersionHasProvenanceFile(chartURL.String())
		if err == nil {
			p.Signed = hasProvenanceFile
		} else {
			w.warn(md, fmt.Errorf("error checking provenance file: %w", err))
		}
	}
	var maintainers []*hub.Maintainer
	for _, entry := range md.Maintainers {
		if entry.Email != "" {
			maintainers = append(maintainers, &hub.Maintainer{
				Name:  entry.Name,
				Email: entry.Email,
			})
		}
	}
	links := make([]*hub.Link, 0, len(md.Sources))
	for _, sourceURL := range md.Sources {
		links = append(links, &hub.Link{
			Name: "source",
			URL:  sourceURL,
		})
	}
	if len(links) > 0 {
		p.Links = links
	}
	if len(maintainers) > 0 {
		p.Maintainers = maintainers
	}
	if strings.Contains(strings.ToLower(md.Name), "operator") {
		p.IsOperator = true
	}
	dependencies := make([]map[string]string, 0, len(md.Dependencies))
	for _, dependency := range md.Dependencies {
		dependencies = append(dependencies, map[string]string{
			"name":       dependency.Name,
			"version":    dependency.Version,
			"repository": dependency.Repository,
		})
	}
	if len(dependencies) > 0 {
		p.Data = map[string]interface{}{
			"dependencies": dependencies,
		}
	}

	// Enrich package with information from annotations
	if err := enrichPackageFromAnnotations(p, md.Annotations); err != nil {
		w.warn(md, fmt.Errorf("error enriching package: %w", err))
	}

	// Register package
	w.logger.Debug().Str("name", md.Name).Str("v", md.Version).Msg("registering package")
	if err := w.svc.Pm.Register(w.svc.Ctx, p); err != nil {
		w.warn(md, fmt.Errorf("error registering package: %w", err))
	}
}

// handleUnregisterJob handles the provided Helm package unregistration job.
// This involves deleting the package version corresponding to a given chart
// version.
func (w *Worker) handleUnregisterJob(j *Job) {
	md := j.ChartVersion.Metadata

	// Unregister package
	p := &hub.Package{
		Name:       md.Name,
		Version:    md.Version,
		Repository: w.r,
	}
	w.logger.Debug().Str("name", p.Name).Str("v", p.Version).Msg("unregistering package")
	if err := w.svc.Pm.Unregister(w.svc.Ctx, p); err != nil {
		w.warn(md, fmt.Errorf("error unregistering package: %w", err))
	}
}

// loadChart loads a chart from a remote archive located at the url provided.
func (w *Worker) loadChart(u *url.URL) (*chart.Chart, error) {
	var r io.Reader

	switch u.Scheme {
	case "http", "https":
		// Get chart content
		req, _ := http.NewRequest("GET", u.String(), nil)
		if u.Host == "github.com" || u.Host == "raw.githubusercontent.com" {
			// Authenticate and rate limit requests to Github
			githubToken := w.svc.Cfg.GetString("tracker.githubToken")
			if githubToken != "" {
				req.Header.Set("Authorization", fmt.Sprintf("token %s", githubToken))
			}
			_ = w.svc.GithubRL.Wait(w.svc.Ctx)
		}
		if w.r.AuthUser != "" || w.r.AuthPass != "" {
			req.SetBasicAuth(w.r.AuthUser, w.r.AuthPass)
		}
		resp, err := w.svc.Hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status code received: %d", resp.StatusCode)
		}
		r = resp.Body
	case "oci":
		// Pull reference layers from OCI registry
		ref := strings.TrimPrefix(u.String(), hub.RepositoryOCIPrefix)
		resolverOptions := docker.ResolverOptions{}
		if w.r.AuthUser != "" || w.r.AuthPass != "" {
			resolverOptions.Authorizer = docker.NewDockerAuthorizer(
				docker.WithAuthCreds(func(string) (string, string, error) {
					return w.r.AuthUser, w.r.AuthPass, nil
				}),
			)
		}
		store := content.NewMemoryStore()
		_, layers, err := oras.Pull(
			ctxo.WithLoggerDiscarded(w.svc.Ctx),
			docker.NewResolver(resolverOptions),
			ref,
			store,
			oras.WithPullEmptyNameAllowed(),
			oras.WithAllowedMediaTypes([]string{helmChartConfigMediaType, helmChartContentLayerMediaType}),
		)
		if err != nil {
			return nil, err
		}

		// Create reader for Helm chart content layer, if available
		for _, layer := range layers {
			if layer.MediaType == helmChartContentLayerMediaType {
				_, b, ok := store.Get(layer)
				if ok {
					r = bytes.NewReader(b)
					break
				}
			}
		}
		if r == nil {
			return nil, errors.New("content layer not found")
		}
	default:
		return nil, repo.ErrSchemeNotSupported
	}

	// Load chart from reader previously setup
	chart, err := loader.LoadArchive(r)
	if err != nil {
		return nil, err
	}
	return chart, nil
}

// chartVersionHasProvenanceFile checks if a chart version has a provenance
// file checking if a .prov file exists for the chart version url provided.
func (w *Worker) chartVersionHasProvenanceFile(u string) (bool, error) {
	req, _ := http.NewRequest("GET", u+".prov", nil)
	if w.r.AuthUser != "" || w.r.AuthPass != "" {
		req.SetBasicAuth(w.r.AuthUser, w.r.AuthPass)
	}
	resp, err := w.svc.Hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	return false, nil
}

// getImage gets the image located at the url provided. If it's a data url the
// image is extracted from it. Otherwise it's downloaded using the url.
func (w *Worker) getImage(imageURL string) ([]byte, error) {
	// Image in data url
	if strings.HasPrefix(imageURL, "data:") {
		dataURL, err := dataurl.DecodeString(imageURL)
		if err != nil {
			return nil, err
		}
		return dataURL.Data, nil
	}

	// Download image using url provided
	req, _ := http.NewRequest("GET", imageURL, nil)
	u, err := url.Parse(imageURL)
	if err != nil {
		return nil, err
	}
	if u.Host == "github.com" || u.Host == "raw.githubusercontent.com" {
		// Authenticate and rate limit requests to Github
		githubToken := w.svc.Cfg.GetString("tracker.githubToken")
		if githubToken != "" {
			req.Header.Set("Authorization", fmt.Sprintf("token %s", githubToken))
		}
		_ = w.svc.GithubRL.Wait(w.svc.Ctx)
	}
	resp, err := w.svc.Hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return ioutil.ReadAll(resp.Body)
	}
	return nil, fmt.Errorf("unexpected status code received: %d", resp.StatusCode)
}

// warn is a helper that sends the error provided to the errors collector and
// logs it as a warning.
func (w *Worker) warn(md *chart.Metadata, err error) {
	err = fmt.Errorf("%s (package: %s version: %s)", err.Error(), md.Name, md.Version)
	w.logger.Warn().Err(err).Send()
	if !md.Deprecated {
		w.svc.Ec.Append(w.r.RepositoryID, err)
	}
}

// getFile returns the file requested from the provided chart.
func getFile(chart *chart.Chart, name string) *chart.File {
	for _, file := range chart.Files {
		if file.Name == name {
			return file
		}
	}
	return nil
}

// enrichPackageFromAnnotations adds some extra information to the package from
// the provided annotations.
func enrichPackageFromAnnotations(p *hub.Package, annotations map[string]string) error {
	// Changes
	if v, ok := annotations[changesAnnotation]; ok {
		var changes []string
		if err := yaml.Unmarshal([]byte(v), &changes); err == nil {
			p.Changes = changes
		}
	}

	// CRDs
	if v, ok := annotations[crdsAnnotation]; ok {
		var crds []interface{}
		if err := yaml.Unmarshal([]byte(v), &crds); err == nil {
			p.CRDs = crds
		}
	}

	// CRDs examples
	if v, ok := annotations[crdsExamplesAnnotation]; ok {
		var crdsExamples []interface{}
		if err := yaml.Unmarshal([]byte(v), &crdsExamples); err == nil {
			p.CRDsExamples = crdsExamples
		} else {
			fmt.Println(err)
		}
	}

	// Images
	if v, ok := annotations[imagesAnnotation]; ok {
		var images []*hub.ContainerImage
		if err := yaml.Unmarshal([]byte(v), &images); err == nil {
			p.ContainersImages = images
		}
	}

	// License
	if v, ok := annotations[licenseAnnotation]; ok && v != "" {
		p.License = v
	}

	// Links
	if v, ok := annotations[linksAnnotation]; ok {
		var links []*hub.Link
		if err := yaml.Unmarshal([]byte(v), &links); err != nil {
			return fmt.Errorf("invalid links value: %s", v)
		}
	LL:
		for _, link := range links {
			for _, pLink := range p.Links {
				if link.URL == pLink.URL {
					pLink.Name = link.Name
					continue LL
				}
			}
			p.Links = append(p.Links, link)
		}
	}

	// Maintainers
	if v, ok := annotations[maintainersAnnotation]; ok {
		var maintainers []*hub.Maintainer
		if err := yaml.Unmarshal([]byte(v), &maintainers); err != nil {
			return fmt.Errorf("invalid maintainers value: %s", v)
		}
	ML:
		for _, maintainer := range maintainers {
			for _, pMaintainer := range p.Maintainers {
				if maintainer.Email == pMaintainer.Email {
					pMaintainer.Name = maintainer.Name
					continue ML
				}
			}
			p.Maintainers = append(p.Maintainers, maintainer)
		}
	}

	// Operator flag
	if v, ok := annotations[operatorAnnotation]; ok {
		isOperator, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("invalid operator value")
		}
		p.IsOperator = isOperator
	}

	// Operator capabilities
	p.Capabilities = annotations[operatorCapabilitiesAnnotation]

	// Prerelease
	if v, ok := annotations[prereleaseAnnotation]; ok {
		prerelease, err := strconv.ParseBool(v)
		if err != nil {
			return errors.New("invalid prerelease value")
		}
		p.Prerelease = prerelease
	}

	// Security updates
	containsSecurityUpdates, err := strconv.ParseBool(annotations[securityUpdatesAnnotation])
	if err == nil {
		p.ContainsSecurityUpdates = containsSecurityUpdates
	}

	return nil
}
