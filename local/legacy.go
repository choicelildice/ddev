package local

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	log "github.com/Sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/fsouza/go-dockerclient"

	"github.com/drud/bootstrap/cli/cms/config"
	"github.com/drud/bootstrap/cli/cms/model"
	"github.com/drud/drud-go/secrets"
	"github.com/drud/drud-go/utils"
)

// LegacyApp implements the LocalApp interface for Legacy Newmedia apps
type LegacyApp struct {
	Name          string
	Environment   string
	AppType       string
	Template      string
	Branch        string
	Repo          string
	Archive       string //absolute path to the downloaded archive
	WebPublicPort int64
	DbPublicPort  int64
}

// RenderComposeYAML returns teh contents of a docker compose config for this app
func (l LegacyApp) RenderComposeYAML() (string, error) {
	var doc bytes.Buffer
	var err error
	templ := template.New("compose template")
	templ, err = templ.Parse(l.Template)
	if err != nil {
		return "", err
	}
	templ.Execute(&doc, map[string]string{
		"image": fmt.Sprintf("drud/nginx-php-fpm-%s", l.AppType),
		"name":  l.ContainerName(),
	})
	return doc.String(), nil
}

// RelPath returns the path from the '.drud' directory to this apps directory
func (l LegacyApp) RelPath() string {
	return path.Join("legacy", fmt.Sprintf("%s-%s", l.Name, l.Environment))
}

// ComposeFileExists returns true if the docker-compose.yaml file exists
func (l LegacyApp) ComposeFileExists() bool {
	composeLOC := path.Join(l.AbsPath(), "docker-compose.yaml")
	if _, err := os.Stat(composeLOC); os.IsNotExist(err) {
		return false
	}
	return true
}

// AbsPath returnt he full path from root to the app directory
func (l LegacyApp) AbsPath() string {
	homedir, err := utils.GetHomeDir()
	if err != nil {
		log.Fatalln(err)
	}
	return path.Join(homedir, ".drud", l.RelPath())
}

// GetName returns the  name for legacy app
func (l LegacyApp) GetName() string {
	return l.Name
}

// ContainerName returns the base name for legacy app containers
func (l LegacyApp) ContainerName() string {
	return fmt.Sprintf("legacy-%s-%s", l.Name, l.Environment)
}

// GetRepoDetails uses the Environment field to get the relevant repo details about an app
func (l LegacyApp) GetRepoDetails() (RepoDetails, error) {
	dbag, err := GetDatabag(l.Name)
	if err != nil {
		return RepoDetails{}, err
	}

	details, err := dbag.GetRepoDetails(l.Environment)
	if err != nil {
		return RepoDetails{}, err
	}

	return details, nil
}

// DatabagExists checks if a databag exists or not
func (l LegacyApp) DatabagExists() bool {
	_, err := GetDatabag(l.Name)
	if err != nil {
		return false
	}
	return true
}

// GetResources downloads external data for this app
func (l *LegacyApp) GetResources() error {
	basePath := l.AbsPath()

	dbag, err := GetDatabag(l.Name)
	if err != nil {
		return err
	}

	s, err := dbag.GetEnv(l.Environment)
	if err != nil {
		return err
	}

	bucket := "nmdarchive"
	if s.AwsBucket != "" {
		bucket = s.AwsBucket
	}

	awsID := s.AwsAccessKey
	awsSecret := s.AwsSecretKey
	if awsID == "" {
		sobj := secrets.Secret{
			Path: "secret/shared/services/awscfg",
		}

		err := sobj.Read()
		if err != nil {
			log.Fatal(err)
		}

		awsID = sobj.Data["accesskey"].(string)
		awsSecret = sobj.Data["secretkey"].(string)
	}

	os.Setenv("AWS_ACCESS_KEY_ID", awsID)
	os.Setenv("AWS_SECRET_ACCESS_KEY", awsSecret)

	svc := s3.New(session.New(&aws.Config{Region: aws.String("us-west-2")}))
	prefix := fmt.Sprintf("%[1]s/%[2]s-%[1]s-", l.Name, l.Environment)

	params := &s3.ListObjectsInput{
		Bucket: aws.String(bucket),
		Prefix: &prefix,
	}

	resp, err := svc.ListObjects(params)
	if err != nil {
		return err
	}

	archive := resp.Contents[len(resp.Contents)-1]
	file, err := os.Create(path.Join(basePath, filepath.Base(*archive.Key)))
	if err != nil {
		log.Fatal("Failed to create file", err)
	}
	defer file.Close()

	downloader := s3manager.NewDownloader(session.New(&aws.Config{Region: aws.String("us-west-2")}))
	numBytes, err := downloader.Download(
		file,
		&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(*archive.Key),
		},
	)
	if err != nil {
		return err
	}

	log.Println("Downloaded file", file.Name(), numBytes, "bytes")
	l.Archive = file.Name()

	return nil
}

// UnpackResources takes the archive from the GetResources method and
// unarchives it. Then the contents are moved to their proper locations.
func (l LegacyApp) UnpackResources() error {
	basePath := l.AbsPath()

	out, err := utils.RunCommand(
		"tar",
		[]string{
			"-xzvf",
			l.Archive,
			"-C", path.Join(basePath, "files"),
			"--exclude=sites/default/settings.php",
			"--exclude=docroot/wp-config.php",
		},
	)
	if err != nil {
		fmt.Println(out)
		return err
	}

	err = os.Remove(l.Archive)
	if err != nil {
		return err
	}

	err = os.Rename(
		path.Join(basePath, "files", l.Name+".sql"),
		path.Join(basePath, "data", l.Name+".sql"),
	)
	if err != nil {
		return err
	}

	rsyncFrom := path.Join(basePath, "files", "docroot")
	rsyncTo := path.Join(basePath, "src", "docroot")
	out, err = utils.RunCommand(
		"rsync",
		[]string{"-avz", "--recursive", "--exclude=profiles", rsyncFrom + "/", rsyncTo},
	)
	if err != nil {
		return fmt.Errorf("%s - %s", err.Error(), string(out))
	}

	return nil
}

// Start initiates docker-compose up
func (l LegacyApp) Start() error {
	basePath := l.AbsPath()
	cmdArgs := []string{"-f", path.Join(basePath, "docker-compose.yaml"), "pull"}
	_, err := utils.RunCommand("docker-compose", cmdArgs)

	if err != nil {
		return err
	}

	return utils.DockerCompose(
		"-f", path.Join(basePath, "docker-compose.yaml"),
		"up",
		"-d",
	)
}

// Stop initiates docker-compose stop
func (l LegacyApp) Stop() error {
	return utils.DockerCompose(
		"-f", path.Join(l.AbsPath(), "docker-compose.yaml"),
		"stop",
	)
}

// SetType determines the app type and sets it
func (l *LegacyApp) SetType() error {
	appType, err := DetermineAppType(l.AbsPath())
	if err != nil {
		return err
	}
	l.AppType = appType
	return nil
}

// Wait ensures that the app appears to be read before returning
func (l *LegacyApp) Wait() (string, error) {
	siteURL := fmt.Sprintf("http://localhost:%d", l.WebPublicPort)
	err := utils.EnsureHTTPStatus(fmt.Sprintf("http://localhost:%d", l.WebPublicPort), "", "", 300, 200)
	if err != nil {
		return "", fmt.Errorf("200 Was not returned from the web container")
	}

	return siteURL, nil
}

// Config creates the apps config file adding thigns like database host, name, and password
// as well as other sensitive data like salts.
func (l *LegacyApp) Config() error {
	basePath := l.AbsPath()

	if l.AppType == "" {
		err := l.SetType()
		if err != nil {
			return err
		}
	}

	dbag, err := GetDatabag(l.Name)
	if err != nil {
		return err
	}

	env, err := dbag.GetEnv(l.Environment)
	if err != nil {
		return err
	}

	l.WebPublicPort, err = GetPodPort(l.ContainerName() + "-web")
	if err != nil {
		return err
	}

	l.DbPublicPort, err = GetPodPort(l.ContainerName() + "-db")
	if err != nil {
		return err
	}

	settingsFilePath := ""
	if l.AppType == "drupal" {
		log.Printf("Drupal site. Creating settings.php file.")
		settingsFilePath = path.Join(basePath, "src", "docroot/sites/default/settings.php")
		drupalConfig := model.NewDrupalConfig()
		drupalConfig.DatabaseHost = "db"
		drupalConfig.HashSalt = env.HashSalt
		if drupalConfig.HashSalt == "" {
			drupalConfig.HashSalt = utils.RandomString(64)
		}

		drupalConfig.DeployURL = "http://localhost:" + strconv.FormatInt(l.WebPublicPort, 10)
		err = config.WriteDrupalConfig(drupalConfig, settingsFilePath)
		if err != nil {
			log.Fatalln(err)
		}

		// Setup a custom settings file for use with drush.
		dbPort, err := GetPodPort(l.ContainerName() + "-db")
		if err != nil {
			return err
		}

		drushSettingsPath := path.Join(basePath, "src", "drush.settings.php")
		drushConfig := model.NewDrushConfig()
		drushConfig.DatabasePort = dbPort
		err = config.WriteDrushConfig(drushConfig, drushSettingsPath)

		if err != nil {
			log.Fatalln(err)
		}
	} else if l.AppType == "wp" {
		log.Printf("WordPress site. Creating wp-config.php file.")
		settingsFilePath = path.Join(basePath, "src", "docroot/wp-config.php")
		wpConfig := model.NewWordpressConfig()
		wpConfig.DatabaseHost = "db"
		wpConfig.DeployURL = fmt.Sprintf("http://localhost:%d", l.WebPublicPort)
		wpConfig.AuthKey = env.AuthKey
		wpConfig.AuthSalt = env.AuthSalt
		wpConfig.LoggedInKey = env.LoggedInKey
		wpConfig.LoggedInSalt = env.LoggedInSalt
		wpConfig.NonceKey = env.NonceKey
		wpConfig.NonceSalt = env.NonceSalt
		wpConfig.SecureAuthKey = env.SecureAuthKey
		wpConfig.SecureAuthSalt = env.SecureAuthSalt
		err = config.WriteWordpressConfig(wpConfig, settingsFilePath)
		if err != nil {
			log.Fatalln(err)
		}
	}
	return nil
}

// Down stops the docker containers for the legacy project.
func (l *LegacyApp) Down() error {
	err := utils.DockerCompose(
		"-f", path.Join(l.AbsPath(), "docker-compose.yaml"),
		"down",
	)
	if err != nil {
		return l.Cleanup()
	}

	return nil

}

// Cleanup will clean up legacy apps even if the composer file has been deleted.
func (l *LegacyApp) Cleanup() error {
	client, _ := GetDockerClient()
	containers, err := client.ListContainers(docker.ListContainersOptions{All: false})

	if err != nil {
		return err
	}

	needle := fmt.Sprintf("legacy-%s-%s", l.Name, l.Environment)
	for _, c := range containers {
		if strings.Contains(c.Names[0], needle) {
			actions := []string{"stop", "rm"}

			for _, action := range actions {
				args := []string{action, c.ID}
				_, err := utils.RunCommand("docker", args)

				if err != nil {
					return fmt.Errorf("Could nnot %s container %s: %s", action, c.Names[0], err)
				}
			}
		}
	}

	return nil
}