/*
Copyright 2021 The KodeRover Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/27149chen/afero"
	"github.com/otiai10/copy"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/yaml"

	configbase "github.com/koderover/zadig/pkg/config"
	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	templatedata "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models/template"
	templatemodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models/template"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	templaterepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb/template"
	commonservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service"
	fsservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/fs"
	templatestore "github.com/koderover/zadig/pkg/microservice/aslan/core/templatestore/repository/models"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/templatestore/repository/mongodb"
	"github.com/koderover/zadig/pkg/setting"
	"github.com/koderover/zadig/pkg/shared/client/systemconfig"
	e "github.com/koderover/zadig/pkg/tool/errors"
	"github.com/koderover/zadig/pkg/tool/log"
	"github.com/koderover/zadig/pkg/types"
	yamlutil "github.com/koderover/zadig/pkg/util/yaml"
)

type HelmService struct {
	ServiceInfos []*models.Service `json:"service_infos"`
	FileInfos    []*types.FileInfo `json:"file_infos"`
	Services     [][]string        `json:"services"`
}

type HelmServiceArgs struct {
	ProductName      string             `json:"product_name"`
	CreateBy         string             `json:"create_by"`
	HelmServiceInfos []*HelmServiceInfo `json:"helm_service_infos"`
}

type HelmServiceInfo struct {
	ServiceName string `json:"service_name"`
	FilePath    string `json:"file_path"`
	FileName    string `json:"file_name"`
	FileContent string `json:"file_content"`
}

type HelmServiceModule struct {
	ServiceModules []*ServiceModule `json:"service_modules"`
	Service        *models.Service  `json:"service,omitempty"`
}

type Chart struct {
	APIVersion string `json:"apiVersion"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	AppVersion string `json:"appVersion"`
}

type helmServiceCreationArgs struct {
	ChartName        string
	ChartVersion     string
	MergedValues     string
	ServiceName      string
	FilePath         string
	ProductName      string
	CreateBy         string
	CodehostID       int
	Owner            string
	Repo             string
	Branch           string
	RepoLink         string
	Source           string
	HelmTemplateName string
	ValuePaths       []string
	ValuesYaml       string
	Variables        []*Variable
}

type ChartTemplateData struct {
	TemplateName      string
	TemplateData      *templatestore.Chart
	ChartName         string
	ChartVersion      string
	DefaultValuesYAML []byte // content of values.yaml in template
}

func ListHelmServices(productName string, log *zap.SugaredLogger) (*HelmService, error) {
	helmService := &HelmService{
		ServiceInfos: []*models.Service{},
		FileInfos:    []*types.FileInfo{},
		Services:     [][]string{},
	}

	opt := &commonrepo.ServiceListOption{
		ProductName: productName,
		Type:        setting.HelmDeployType,
	}

	services, err := commonrepo.NewServiceColl().ListMaxRevisions(opt)
	if err != nil {
		log.Errorf("[helmService.list] err:%v", err)
		return nil, e.ErrListTemplate.AddErr(err)
	}
	helmService.ServiceInfos = services

	if len(services) > 0 {
		fis, err := loadServiceFileInfos(services[0].ProductName, services[0].ServiceName, "")
		if err != nil {
			log.Errorf("Failed to load service file info, err: %s", err)
			return nil, e.ErrListTemplate.AddErr(err)
		}
		helmService.FileInfos = fis
	}
	project, err := templaterepo.NewProductColl().Find(productName)
	if err != nil {
		log.Errorf("Failed to find project info, err: %s", err)
		return nil, e.ErrListTemplate.AddErr(err)
	}
	helmService.Services = project.Services

	return helmService, nil
}

func GetHelmServiceModule(serviceName, productName string, revision int64, log *zap.SugaredLogger) (*HelmServiceModule, error) {

	serviceTemplate, err := commonservice.GetServiceTemplate(serviceName, setting.HelmDeployType, productName, setting.ProductStatusDeleting, revision, log)
	if err != nil {
		return nil, err
	}
	helmServiceModule := new(HelmServiceModule)
	serviceModules := make([]*ServiceModule, 0)
	for _, container := range serviceTemplate.Containers {
		serviceModule := new(ServiceModule)
		serviceModule.Container = container
		buildObj, _ := commonrepo.NewBuildColl().Find(&commonrepo.BuildFindOption{ProductName: productName, ServiceName: serviceName, Targets: []string{container.Name}})
		if buildObj != nil {
			serviceModule.BuildName = buildObj.Name
		}
		serviceModules = append(serviceModules, serviceModule)
	}
	helmServiceModule.Service = serviceTemplate
	helmServiceModule.ServiceModules = serviceModules
	return helmServiceModule, err
}

func GetFilePath(serviceName, productName, dir string, _ *zap.SugaredLogger) ([]*types.FileInfo, error) {
	return loadServiceFileInfos(productName, serviceName, dir)
}

func GetFileContent(serviceName, productName, filePath, fileName string, log *zap.SugaredLogger) (string, error) {
	base := config.LocalServicePath(productName, serviceName)

	svc, err := commonrepo.NewServiceColl().Find(&commonrepo.ServiceFindOption{
		ProductName: productName,
		ServiceName: serviceName,
	})
	if err != nil {
		return "", e.ErrFileContent.AddDesc(err.Error())
	}

	err = commonservice.PreLoadServiceManifests(base, svc)
	if err != nil {
		return "", e.ErrFileContent.AddDesc(err.Error())
	}

	file := filepath.Join(base, serviceName, filePath, fileName)
	fileContent, err := os.ReadFile(file)
	if err != nil {
		log.Errorf("Failed to read file %s, err: %s", file, err)
		return "", e.ErrFileContent.AddDesc(err.Error())
	}

	return string(fileContent), nil
}

func prepareChartTemplateData(templateName string, logger *zap.SugaredLogger) (*ChartTemplateData, error) {
	templateChart, err := mongodb.NewChartColl().Get(templateName)
	if err != nil {
		logger.Errorf("Failed to get chart template %s, err: %s", templateName, err)
		return nil, fmt.Errorf("failed to get chart template: %s", templateName)
	}

	// get chart template from local disk
	localBase := configbase.LocalChartTemplatePath(templateName)
	s3Base := configbase.ObjectStorageChartTemplatePath(templateName)
	if err = fsservice.PreloadFiles(templateName, localBase, s3Base, logger); err != nil {
		logger.Errorf("Failed to download template %s, err: %s", templateName, err)
		return nil, err
	}

	base := filepath.Base(templateChart.Path)
	defaultValuesFile := filepath.Join(localBase, base, setting.ValuesYaml)
	defaultValues, _ := os.ReadFile(defaultValuesFile)

	chartFilePath := filepath.Join(localBase, base, setting.ChartYaml)
	chartFileContent, err := os.ReadFile(chartFilePath)
	if err != nil {
		logger.Errorf("Failed to read chartfile template %s, err: %s", templateName, err)
		return nil, err
	}
	chart := new(Chart)
	if err = yaml.Unmarshal(chartFileContent, chart); err != nil {
		logger.Errorf("Failed to unmarshal chart yaml %s, err: %s", setting.ChartYaml, err)
		return nil, err
	}

	return &ChartTemplateData{
		TemplateName:      templateName,
		TemplateData:      templateChart,
		ChartName:         chart.Name,
		ChartVersion:      chart.Version,
		DefaultValuesYAML: defaultValues,
	}, nil
}

func CreateOrUpdateHelmService(projectName string, args *HelmServiceCreationArgs, logger *zap.SugaredLogger) (*BulkHelmServiceCreationResponse, error) {
	switch args.Source {
	case LoadFromRepo, LoadFromPublicRepo:
		return CreateOrUpdateHelmServiceFromGitRepo(projectName, args, logger)
	case LoadFromChartTemplate:
		return CreateOrUpdateHelmServiceFromChartTemplate(projectName, args, logger)
	default:
		return nil, fmt.Errorf("invalid source")
	}
}

func CreateOrUpdateHelmServiceFromChartTemplate(projectName string, args *HelmServiceCreationArgs, logger *zap.SugaredLogger) (*BulkHelmServiceCreationResponse, error) {
	templateArgs, ok := args.CreateFrom.(*CreateFromChartTemplate)
	if !ok {
		return nil, fmt.Errorf("invalid argument")
	}

	templateChartInfo, err := prepareChartTemplateData(templateArgs.TemplateName, logger)
	if err != nil {
		return nil, err
	}

	var values [][]byte
	if len(templateChartInfo.DefaultValuesYAML) > 0 {
		//render variables
		renderedYaml, err := renderVariablesToYaml(string(templateChartInfo.DefaultValuesYAML), projectName, args.Name, templateArgs.Variables)
		if err != nil {
			return nil, err
		}
		values = append(values, []byte(renderedYaml))
	}

	if len(templateArgs.ValuesYAML) > 0 {
		values = append(values, []byte(templateArgs.ValuesYAML))
	}

	localBase := configbase.LocalChartTemplatePath(templateArgs.TemplateName)
	base := filepath.Base(templateChartInfo.TemplateData.Path)

	// copy template to service path and update the values.yaml
	from := filepath.Join(localBase, base)
	to := filepath.Join(config.LocalServicePath(projectName, args.Name), args.Name)
	// remove old files
	if err = os.RemoveAll(to); err != nil {
		logger.Errorf("Failed to remove dir %s, err: %s", to, err)
		return nil, err
	}
	if err = copy.Copy(from, to); err != nil {
		logger.Errorf("Failed to copy file from %s to %s, err: %s", from, to, err)
		return nil, err
	}

	merged, err := yamlutil.Merge(values)
	if err != nil {
		logger.Errorf("Failed to merge values, err: %s", err)
		return nil, err
	}

	if err = os.WriteFile(filepath.Join(to, setting.ValuesYaml), merged, 0644); err != nil {
		logger.Errorf("Failed to write values, err: %s", err)
		return nil, err
	}

	fsTree := os.DirFS(config.LocalServicePath(projectName, args.Name))
	ServiceS3Base := config.ObjectStorageServicePath(projectName, args.Name)
	if err = fsservice.ArchiveAndUploadFilesToS3(fsTree, args.Name, ServiceS3Base, logger); err != nil {
		logger.Errorf("Failed to upload files for service %s in project %s, err: %s", args.Name, projectName, err)
		return nil, err
	}

	svc, err := createOrUpdateHelmService(
		fsTree,
		&helmServiceCreationArgs{
			ChartName:        templateChartInfo.ChartName,
			ChartVersion:     templateChartInfo.ChartVersion,
			MergedValues:     string(merged),
			ServiceName:      args.Name,
			FilePath:         to,
			ProductName:      projectName,
			CreateBy:         args.CreatedBy,
			Source:           setting.SourceFromChartTemplate,
			HelmTemplateName: templateArgs.TemplateName,
			ValuesYaml:       templateArgs.ValuesYAML,
			Variables:        templateArgs.Variables,
		},
		logger,
	)
	if err != nil {
		logger.Errorf("Failed to create service %s in project %s, error: %s", args.Name, projectName, err)
		return nil, err
	}

	compareHelmVariable([]*templatemodels.RenderChart{
		{ServiceName: args.Name,
			ChartVersion: svc.HelmChart.Version,
			ValuesYaml:   svc.HelmChart.ValuesYaml,
		},
	}, projectName, args.CreatedBy, logger)

	return &BulkHelmServiceCreationResponse{
		SuccessServices: []string{args.Name},
	}, nil
}

func getCodehostType(repoArgs *CreateFromRepo, repoLink string) (string, *systemconfig.CodeHost, error) {
	if repoLink != "" {
		return setting.SourceFromPublicRepo, nil, nil
	}
	ch, err := systemconfig.New().GetCodeHost(repoArgs.CodehostID)
	if err != nil {
		log.Errorf("Failed to get codeHost by id %d, err: %s", repoArgs.CodehostID, err.Error())
		return "", ch, err
	}
	return ch.Type, ch, nil
}

func CreateOrUpdateHelmServiceFromGitRepo(projectName string, args *HelmServiceCreationArgs, log *zap.SugaredLogger) (*BulkHelmServiceCreationResponse, error) {
	var err error
	var repoLink string
	repoArgs, ok := args.CreateFrom.(*CreateFromRepo)
	if !ok {
		publicArgs, ok := args.CreateFrom.(*CreateFromPublicRepo)
		if !ok {
			return nil, fmt.Errorf("invalid argument")
		}

		repoArgs, err = PublicRepoToPrivateRepoArgs(publicArgs)
		if err != nil {
			log.Errorf("Failed to parse repo args %+v, err: %s", publicArgs, err)
			return nil, err
		}

		repoLink = publicArgs.RepoLink
	}

	response := &BulkHelmServiceCreationResponse{}
	source, codehostInfo, err := getCodehostType(repoArgs, repoLink)
	if err != nil {
		log.Errorf("Failed to get source form repo data %+v, err: %s", *repoArgs, err.Error())
		return nil, err
	}

	helmRenderCharts := make([]*templatemodels.RenderChart, 0, len(repoArgs.Paths))

	var wg wait.Group
	var mux sync.RWMutex
	for _, p := range repoArgs.Paths {
		filePath := strings.TrimLeft(p, "/")
		wg.Start(func() {
			var (
				serviceName  string
				chartVersion string
				valuesYAML   []byte
				finalErr     error
			)
			defer func() {
				mux.Lock()
				if finalErr != nil {
					response.FailedServices = append(response.FailedServices, &FailedService{
						Path:  filePath,
						Error: finalErr.Error(),
					})
				} else {
					response.SuccessServices = append(response.SuccessServices, serviceName)
				}
				mux.Unlock()
			}()

			log.Infof("Loading chart under path %s", filePath)

			fsTree, err := fsservice.DownloadFilesFromSource(
				&fsservice.DownloadFromSourceArgs{CodehostID: repoArgs.CodehostID, Owner: repoArgs.Owner, Repo: repoArgs.Repo, Path: filePath, Branch: repoArgs.Branch, RepoLink: repoLink},
				func(chartTree afero.Fs) (string, error) {
					var err error
					serviceName, chartVersion, err = readChartYAML(afero.NewIOFS(chartTree), filepath.Base(filePath), log)
					if err != nil {
						return serviceName, err
					}
					valuesYAML, err = readValuesYAML(afero.NewIOFS(chartTree), filepath.Base(filePath), log)
					return serviceName, err
				})
			if err != nil {
				log.Errorf("Failed to download files from source, err %s", err)
				finalErr = e.ErrCreateTemplate.AddErr(err)
				return
			}

			log.Info("Found valid chart, Starting to save and upload files")

			// save files to disk and upload them to s3
			if err = commonservice.SaveAndUploadService(projectName, serviceName, fsTree); err != nil {
				log.Errorf("Failed to save or upload files for service %s in project %s, error: %s", serviceName, projectName, err)
				finalErr = e.ErrCreateTemplate.AddErr(err)
				return
			}

			if source != setting.SourceFromPublicRepo && codehostInfo != nil {
				repoLink = fmt.Sprintf("%s/%s/%s/%s/%s/%s", codehostInfo.Address, repoArgs.Owner, repoArgs.Repo, "tree", repoArgs.Branch, filePath)
			}

			svc, err := createOrUpdateHelmService(
				fsTree,
				&helmServiceCreationArgs{
					ChartName:    serviceName,
					ChartVersion: chartVersion,
					MergedValues: string(valuesYAML),
					ServiceName:  serviceName,
					FilePath:     filePath,
					ProductName:  projectName,
					CreateBy:     args.CreatedBy,
					CodehostID:   repoArgs.CodehostID,
					Owner:        repoArgs.Owner,
					Repo:         repoArgs.Repo,
					Branch:       repoArgs.Branch,
					RepoLink:     repoLink,
					Source:       source,
				},
				log,
			)
			if err != nil {
				log.Errorf("Failed to create service %s in project %s, error: %s", serviceName, projectName, err)
				finalErr = e.ErrCreateTemplate.AddErr(err)
				return
			}

			helmRenderCharts = append(helmRenderCharts, &templatemodels.RenderChart{
				ServiceName:  serviceName,
				ChartVersion: svc.HelmChart.Version,
				ValuesYaml:   svc.HelmChart.ValuesYaml,
			})
		})
	}

	wg.Wait()

	compareHelmVariable(helmRenderCharts, projectName, args.CreatedBy, log)
	return response, nil
}

func CreateOrUpdateBulkHelmService(projectName string, args *BulkHelmServiceCreationArgs, logger *zap.SugaredLogger) (*BulkHelmServiceCreationResponse, error) {
	switch args.Source {
	case LoadFromChartTemplate:
		return CreateOrUpdateBulkHelmServiceFromTemplate(projectName, args, logger)
	default:
		return nil, fmt.Errorf("invalid source")
	}
}

func CreateOrUpdateBulkHelmServiceFromTemplate(projectName string, args *BulkHelmServiceCreationArgs, logger *zap.SugaredLogger) (*BulkHelmServiceCreationResponse, error) {
	templateArgs, ok := args.CreateFrom.(*CreateFromChartTemplate)
	if !ok {
		return nil, fmt.Errorf("invalid argument")
	}

	if args.ValuesData == nil || args.ValuesData.GitRepoConfig == nil || len(args.ValuesData.GitRepoConfig.ValuesPaths) == 0 {
		return nil, fmt.Errorf("invalid argument, missing values")
	}

	templateChartData, err := prepareChartTemplateData(templateArgs.TemplateName, logger)
	if err != nil {
		return nil, err
	}

	localBase := configbase.LocalChartTemplatePath(templateArgs.TemplateName)
	base := filepath.Base(templateChartData.TemplateData.Path)
	// copy template to service path and update the values.yaml
	from := filepath.Join(localBase, base)

	//record errors for every service
	failedServiceMap := &sync.Map{}
	renderChartMap := &sync.Map{}

	wg := sync.WaitGroup{}
	// run goroutines to speed up
	for _, singlePath := range args.ValuesData.GitRepoConfig.ValuesPaths {
		wg.Add(1)
		go func(repoConfig *commonservice.RepoConfig, path string) {
			defer wg.Done()
			renderChart, err := handleSingleService(projectName, repoConfig, path, from, args.CreatedBy, templateChartData, logger)
			if err != nil {
				failedServiceMap.Store(path, err.Error())
			} else {
				renderChartMap.Store(renderChart.ServiceName, renderChart)
			}
		}(args.ValuesData.GitRepoConfig, singlePath)
	}

	wg.Wait()

	resp := &BulkHelmServiceCreationResponse{
		SuccessServices: make([]string, 0),
		FailedServices:  make([]*FailedService, 0),
	}

	renderChars := make([]*templatemodels.RenderChart, 0)

	renderChartMap.Range(func(key, value interface{}) bool {
		resp.SuccessServices = append(resp.SuccessServices, key.(string))
		renderChars = append(renderChars, value.(*templatemodels.RenderChart))
		return true
	})

	failedServiceMap.Range(func(key, value interface{}) bool {
		resp.FailedServices = append(resp.FailedServices, &FailedService{
			Path:  key.(string),
			Error: value.(string),
		})
		return true
	})

	compareHelmVariable(renderChars, projectName, args.CreatedBy, logger)

	return resp, nil
}

func handleSingleService(projectName string, repoConfig *commonservice.RepoConfig, path, fromPath, createBy string,
	templateChartData *ChartTemplateData, logger *zap.SugaredLogger) (*templatemodels.RenderChart, error) {

	valuesYAML, err := fsservice.DownloadFileFromSource(&fsservice.DownloadFromSourceArgs{
		CodehostID: repoConfig.CodehostID,
		Owner:      repoConfig.Owner,
		Repo:       repoConfig.Repo,
		Path:       path,
		Branch:     repoConfig.Branch,
	})
	if err != nil {
		return nil, err
	}

	if len(valuesYAML) == 0 {
		return nil, fmt.Errorf("values.yaml is empty")
	}

	values := [][]byte{templateChartData.DefaultValuesYAML, valuesYAML}
	mergedValues, err := yamlutil.Merge(values)
	if err != nil {
		logger.Errorf("Failed to merge values, err: %s", err)
		return nil, err
	}

	serviceName := filepath.Base(path)
	serviceName = strings.TrimSuffix(serviceName, filepath.Ext(serviceName))

	to := filepath.Join(config.LocalServicePath(projectName, serviceName), serviceName)
	// remove old files
	if err = os.RemoveAll(to); err != nil {
		logger.Errorf("Failed to remove dir %s, err: %s", to, err)
		return nil, err
	}
	if err = copy.Copy(fromPath, to); err != nil {
		logger.Errorf("Failed to copy file from %s to %s, err: %s", fromPath, to, err)
		return nil, err
	}

	// write values.yaml file
	if err = os.WriteFile(filepath.Join(to, setting.ValuesYaml), mergedValues, 0644); err != nil {
		logger.Errorf("Failed to write values, err: %s", err)
		return nil, err
	}

	fsTree := os.DirFS(config.LocalServicePath(projectName, serviceName))
	ServiceS3Base := config.ObjectStorageServicePath(projectName, serviceName)
	if err = fsservice.ArchiveAndUploadFilesToS3(fsTree, serviceName, ServiceS3Base, logger); err != nil {
		logger.Errorf("Failed to upload files for service %s in project %s, err: %s", serviceName, projectName, err)
		return nil, err
	}

	_, err = createOrUpdateHelmService(
		fsTree,
		&helmServiceCreationArgs{
			ChartName:        templateChartData.ChartName,
			ChartVersion:     templateChartData.ChartVersion,
			MergedValues:     string(mergedValues),
			ServiceName:      serviceName,
			FilePath:         to,
			ProductName:      projectName,
			CreateBy:         createBy,
			CodehostID:       repoConfig.CodehostID,
			Source:           setting.SourceFromChartTemplate,
			HelmTemplateName: templateChartData.TemplateName,
			ValuePaths:       []string{path},
			ValuesYaml:       string(valuesYAML),
		},
		logger,
	)
	if err != nil {
		logger.Errorf("Failed to create service %s in project %s, error: %s", serviceName, projectName, err)
		return nil, err
	}

	return &templatemodels.RenderChart{
		ServiceName:  serviceName,
		ChartVersion: templateChartData.ChartVersion,
		ValuesYaml:   string(mergedValues),
	}, nil
}

func readChartYAML(chartTree fs.FS, base string, logger *zap.SugaredLogger) (string, string, error) {
	chartFile, err := fs.ReadFile(chartTree, filepath.Join(base, setting.ChartYaml))
	if err != nil {
		logger.Errorf("Failed to read %s, err: %s", setting.ChartYaml, err)
		return "", "", err
	}
	chart := new(Chart)
	if err = yaml.Unmarshal(chartFile, chart); err != nil {
		log.Errorf("Failed to unmarshal yaml %s, err: %s", setting.ChartYaml, err)
		return "", "", err
	}

	return chart.Name, chart.Version, nil
}

func readValuesYAML(chartTree fs.FS, base string, logger *zap.SugaredLogger) ([]byte, error) {
	content, err := fs.ReadFile(chartTree, filepath.Join(base, setting.ValuesYaml))
	if err != nil {
		logger.Errorf("Failed to read %s, err: %s", setting.ValuesYaml, err)
		return nil, err
	}
	return content, nil
}

func geneCreationDetail(args *helmServiceCreationArgs) interface{} {
	switch args.Source {
	case setting.SourceFromGitlab,
		setting.SourceFromGithub,
		setting.SourceFromGerrit,
		setting.SourceFromCodeHub:
		return &models.CreateFromRepo{
			GitRepoConfig: &templatemodels.GitRepoConfig{
				CodehostID: args.CodehostID,
				Owner:      args.Owner,
				Repo:       args.Repo,
				Branch:     args.Branch,
			},
			LoadPath: args.FilePath,
		}
	case setting.SourceFromPublicRepo:
		return &models.CreateFromPublicRepo{
			RepoLink: args.RepoLink,
			LoadPath: args.FilePath,
		}
	case setting.SourceFromChartTemplate:
		yamlData := &templatedata.CustomYaml{
			YamlContent: args.ValuesYaml,
		}
		variables := make([]*models.Variable, 0, len(args.Variables))
		for _, variable := range args.Variables {
			variables = append(variables, &models.Variable{
				Key:   variable.Key,
				Value: variable.Value,
			})
		}
		return &models.CreateFromChartTemplate{
			YamlData:     yamlData,
			TemplateName: args.HelmTemplateName,
			ServiceName:  args.ServiceName,
			Variables:    variables,
		}
	}
	return nil
}

func renderVariablesToYaml(valuesYaml string, productName, serviceName string, variables []*Variable) (string, error) {
	valuesYaml = strings.Replace(valuesYaml, setting.TemplateVariableProduct, productName, -1)
	valuesYaml = strings.Replace(valuesYaml, setting.TemplateVariableService, serviceName, -1)

	// build replace data
	valuesMap := make(map[string]interface{})
	for _, variable := range variables {
		valuesMap[variable.Key] = variable.Value
	}

	tmpl, err := template.New("values").Parse(valuesYaml)
	if err != nil {
		log.Errorf("failed to parse template, err %s valuesYaml %s", err, valuesYaml)
		return "", errors.Wrapf(err, "failed to parse template, err %s", err)
	}

	buf := bytes.NewBufferString("")
	err = tmpl.Execute(buf, valuesMap)
	if err != nil {
		log.Errorf("failed to render values content, err %s", err)
		return "", fmt.Errorf("failed to render variables")
	}
	valuesYaml = buf.String()
	return valuesYaml, nil
}

func createOrUpdateHelmService(fsTree fs.FS, args *helmServiceCreationArgs, logger *zap.SugaredLogger) (*models.Service, error) {
	chartName, chartVersion, err := readChartYAML(fsTree, args.ServiceName, logger)
	if err != nil {
		logger.Errorf("Failed to read chart.yaml, err %s", err)
		return nil, err
	}

	valuesYaml := args.MergedValues
	valuesMap := make(map[string]interface{})
	err = yaml.Unmarshal([]byte(valuesYaml), &valuesMap)
	if err != nil {
		logger.Errorf("Failed to unmarshall yaml, err %s", err)
		return nil, err
	}

	serviceTemplate := fmt.Sprintf(setting.ServiceTemplateCounterName, args.ServiceName, args.ProductName)
	rev, err := commonrepo.NewCounterColl().GetNextSeq(serviceTemplate)
	if err != nil {
		logger.Errorf("Failed to get next revision for service %s, err: %s", args.ServiceName, err)
		return nil, err
	}
	if err = commonrepo.NewServiceColl().Delete(args.ServiceName, setting.HelmDeployType, args.ProductName, setting.ProductStatusDeleting, rev); err != nil {
		logger.Warnf("Failed to delete stale service %s with revision %d, err: %s", args.ServiceName, rev, err)
	}

	containerList, err := commonservice.ParseImagesForProductService(valuesMap, args.ServiceName, args.ProductName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse service from yaml")
	}

	serviceObj := &models.Service{
		ServiceName: args.ServiceName,
		Type:        setting.HelmDeployType,
		Revision:    rev,
		ProductName: args.ProductName,
		Visibility:  setting.PrivateVisibility,
		CreateTime:  time.Now().Unix(),
		CreateBy:    args.CreateBy,
		Containers:  containerList,
		CodehostID:  args.CodehostID,
		RepoOwner:   args.Owner,
		RepoName:    args.Repo,
		BranchName:  args.Branch,
		LoadPath:    args.FilePath,
		SrcPath:     args.RepoLink,
		CreateFrom:  geneCreationDetail(args),
		Source:      args.Source,
		HelmChart: &models.HelmChart{
			Name:       chartName,
			Version:    chartVersion,
			ValuesYaml: valuesYaml,
		},
	}

	log.Infof("Starting to create service %s with revision %d", args.ServiceName, rev)

	if err = commonrepo.NewServiceColl().Create(serviceObj); err != nil {
		log.Errorf("Failed to create service %s error: %s", args.ServiceName, err)
		return nil, err
	}

	if err = templaterepo.NewProductColl().AddService(args.ProductName, args.ServiceName); err != nil {
		log.Errorf("Failed to add service %s to project %s, err: %s", args.ProductName, args.ServiceName, err)
		return nil, err
	}

	return serviceObj, nil
}

func loadServiceFileInfos(productName, serviceName, dir string) ([]*types.FileInfo, error) {
	base := config.LocalServicePath(productName, serviceName)

	svc, err := commonrepo.NewServiceColl().Find(&commonrepo.ServiceFindOption{
		ProductName: productName,
		ServiceName: serviceName,
	})
	if err != nil {
		return nil, e.ErrFilePath.AddDesc(err.Error())
	}

	err = commonservice.PreLoadServiceManifests(base, svc)
	if err != nil {
		return nil, e.ErrFilePath.AddDesc(err.Error())
	}
	var fis []*types.FileInfo
	files, err := os.ReadDir(filepath.Join(base, serviceName, dir))
	if err != nil {
		return nil, e.ErrFilePath.AddDesc(err.Error())
	}

	for _, file := range files {
		info, _ := file.Info()
		if info == nil {
			continue
		}
		fi := &types.FileInfo{
			Parent:  dir,
			Name:    file.Name(),
			Size:    info.Size(),
			Mode:    file.Type(),
			ModTime: info.ModTime().Unix(),
			IsDir:   file.IsDir(),
		}

		fis = append(fis, fi)
	}
	return fis, nil
}

// UpdateHelmService TODO need to be deprecated
func UpdateHelmService(args *HelmServiceArgs, log *zap.SugaredLogger) error {
	var serviceNames []string
	for _, helmServiceInfo := range args.HelmServiceInfos {
		serviceNames = append(serviceNames, helmServiceInfo.ServiceName)

		opt := &commonrepo.ServiceFindOption{
			ProductName: args.ProductName,
			ServiceName: helmServiceInfo.ServiceName,
			Type:        setting.HelmDeployType,
		}
		preServiceTmpl, err := commonrepo.NewServiceColl().Find(opt)
		if err != nil {
			return e.ErrUpdateTemplate.AddDesc(err.Error())
		}

		base := config.LocalServicePath(args.ProductName, helmServiceInfo.ServiceName)
		if err = commonservice.PreLoadServiceManifests(base, preServiceTmpl); err != nil {
			return e.ErrUpdateTemplate.AddDesc(err.Error())
		}

		filePath := filepath.Join(base, helmServiceInfo.ServiceName, helmServiceInfo.FilePath, helmServiceInfo.FileName)
		if err = os.WriteFile(filePath, []byte(helmServiceInfo.FileContent), 0644); err != nil {
			log.Errorf("Failed to write file %s, err: %s", filePath, err)
			return e.ErrUpdateTemplate.AddDesc(err.Error())
		}

		// TODO：use yaml compare instead of just comparing the characters
		// TODO service variables
		if helmServiceInfo.FileName == setting.ValuesYaml && preServiceTmpl.HelmChart.ValuesYaml != helmServiceInfo.FileContent {
			var valuesMap map[string]interface{}
			if err = yaml.Unmarshal([]byte(helmServiceInfo.FileContent), &valuesMap); err != nil {
				return e.ErrCreateTemplate.AddDesc("values.yaml解析失败")
			}

			containerList, err := commonservice.ParseImagesForProductService(valuesMap, preServiceTmpl.ServiceName, preServiceTmpl.ProductName)
			if err != nil {
				return e.ErrUpdateTemplate.AddErr(errors.Wrapf(err, "failed to parse images from yaml"))
			}

			preServiceTmpl.Containers = containerList
			preServiceTmpl.HelmChart.ValuesYaml = helmServiceInfo.FileContent

			//修改helm renderset
			renderOpt := &commonrepo.RenderSetFindOption{Name: args.ProductName}
			if rs, err := commonrepo.NewRenderSetColl().Find(renderOpt); err == nil {
				for _, chartInfo := range rs.ChartInfos {
					if chartInfo.ServiceName == helmServiceInfo.ServiceName {
						chartInfo.ValuesYaml = helmServiceInfo.FileContent
						break
					}
				}
				if err = commonrepo.NewRenderSetColl().Update(rs); err != nil {
					log.Errorf("[renderset.update] err:%v", err)
				}
			}
		} else if helmServiceInfo.FileName == setting.ChartYaml {
			chart := new(Chart)
			if err = yaml.Unmarshal([]byte(helmServiceInfo.FileContent), chart); err != nil {
				return e.ErrCreateTemplate.AddDesc(fmt.Sprintf("解析%s失败", setting.ChartYaml))
			}
			if preServiceTmpl.HelmChart.Version != chart.Version {
				preServiceTmpl.HelmChart.Version = chart.Version

				//修改helm renderset
				renderOpt := &commonrepo.RenderSetFindOption{Name: args.ProductName}
				if rs, err := commonrepo.NewRenderSetColl().Find(renderOpt); err == nil {
					for _, chartInfo := range rs.ChartInfos {
						if chartInfo.ServiceName == helmServiceInfo.ServiceName {
							chartInfo.ChartVersion = chart.Version
							break
						}
					}
					if err = commonrepo.NewRenderSetColl().Update(rs); err != nil {
						log.Errorf("[renderset.update] err:%v", err)
					}
				}
			}
		}

		preServiceTmpl.CreateBy = args.CreateBy
		serviceTemplate := fmt.Sprintf(setting.ServiceTemplateCounterName, helmServiceInfo.ServiceName, preServiceTmpl.ProductName)
		rev, err := commonrepo.NewCounterColl().GetNextSeq(serviceTemplate)
		if err != nil {
			return fmt.Errorf("get next helm service revision error: %v", err)
		}

		preServiceTmpl.Revision = rev
		if err := commonrepo.NewServiceColl().Delete(helmServiceInfo.ServiceName, setting.HelmDeployType, args.ProductName, setting.ProductStatusDeleting, preServiceTmpl.Revision); err != nil {
			log.Errorf("helmService.update delete %s error: %v", helmServiceInfo.ServiceName, err)
		}

		if err := commonrepo.NewServiceColl().Create(preServiceTmpl); err != nil {
			log.Errorf("helmService.update serviceName:%s error:%v", helmServiceInfo.ServiceName, err)
			return e.ErrUpdateTemplate.AddDesc(err.Error())
		}
	}

	for _, serviceName := range serviceNames {
		s3Base := config.ObjectStorageServicePath(args.ProductName, serviceName)
		if err := fsservice.ArchiveAndUploadFilesToS3(os.DirFS(config.LocalServicePath(args.ProductName, serviceName)), serviceName, s3Base, log); err != nil {
			return e.ErrUpdateTemplate.AddDesc(err.Error())
		}
	}

	return nil
}

// compareHelmVariable 比较helm变量是否有改动，是否需要添加新的renderSet
func compareHelmVariable(chartInfos []*templatemodels.RenderChart, productName, createdBy string, log *zap.SugaredLogger) {
	// 对比上个版本的renderset，新增一个版本
	latestChartInfos := make([]*templatemodels.RenderChart, 0)
	renderOpt := &commonrepo.RenderSetFindOption{Name: productName}
	if latestDefaultRenderSet, err := commonrepo.NewRenderSetColl().Find(renderOpt); err == nil {
		latestChartInfos = latestDefaultRenderSet.ChartInfos
	}

	currentChartInfoMap := make(map[string]*templatemodels.RenderChart)
	for _, chartInfo := range chartInfos {
		currentChartInfoMap[chartInfo.ServiceName] = chartInfo
	}

	mixtureChartInfos := make([]*templatemodels.RenderChart, 0)
	for _, latestChartInfo := range latestChartInfos {
		//如果新的里面存在就拿新的数据替换，不存在就还使用老的数据
		if currentChartInfo, isExist := currentChartInfoMap[latestChartInfo.ServiceName]; isExist {
			mixtureChartInfos = append(mixtureChartInfos, currentChartInfo)
			delete(currentChartInfoMap, latestChartInfo.ServiceName)
			continue
		}
		mixtureChartInfos = append(mixtureChartInfos, latestChartInfo)
	}

	//把新增的服务添加到新的slice里面
	for _, chartInfo := range currentChartInfoMap {
		mixtureChartInfos = append(mixtureChartInfos, chartInfo)
	}

	//添加renderset
	if err := commonservice.CreateHelmRenderSet(
		&models.RenderSet{
			Name:        productName,
			Revision:    0,
			ProductTmpl: productName,
			UpdateBy:    createdBy,
			ChartInfos:  mixtureChartInfos,
		}, log,
	); err != nil {
		log.Errorf("helmService.Create CreateHelmRenderSet error: %v", err)
	}
}
