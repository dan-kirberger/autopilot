package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cloudfoundry/cli/plugin"
	"github.com/concourse/autopilot/rewind"
)

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stdout, "error:", err)
		os.Exit(1)
	}
}

func main() {
	plugin.Start(&AutopilotPlugin{})
}

type AutopilotPlugin struct{}

func venerableAppName(appName string) string {
	return fmt.Sprintf("%s-venerable", appName)
}

func rollbackAppName(appName string) string {
	return fmt.Sprintf("%s-rollback", appName)
}

func getActionsForRollback(appName string, appRepo *ApplicationRepo, args []string) []rewind.Action {
	return []rewind.Action{
		//Rename live app
		{
			Forward: func() error {
				return appRepo.RenameApplication(appName, rollbackAppName(appName))
			},
			ReversePrevious: func() error {
				return appRepo.RenameApplication(rollbackAppName(appName), appName)
			},
		},
		//Rename venerable app
		{
			Forward: func() error {
				return appRepo.RenameApplication(venerableAppName(appName), appName)
			},
			ReversePrevious: func() error {
				return appRepo.RenameApplication(appName, venerableAppName(appName))
			},
		},
		//Start rollback app
		{
			Forward: func() error {
				return appRepo.StartApplication(appName)

			},
		},
		//Delete rolled back app
		{
			Forward: func() error {
				return appRepo.DeleteApplication(rollbackAppName(appName))
			},
		},
	}
}

func getActionsForPush(appRepo *ApplicationRepo, args []string) []rewind.Action {
	appName, manifestPath, appPath, options, err := ParseArgs(args)
	fatalIf(err)

	appExists, err := appRepo.DoesAppExist(appName)
	fatalIf(err)

	if appExists {
		return getActionsForExistingApp(appRepo, appName, manifestPath, appPath, options)
	} else {
		return getActionsForNewApp(appRepo, appName, manifestPath, appPath)
	}
}

func getActionsForExistingApp(appRepo *ApplicationRepo, appName, manifestPath, appPath string, options AutopilotOptions) []rewind.Action {
	return []rewind.Action{
		// delete old version if it still exists
		{
			Forward: func() error {
				appExists, err := appRepo.DoesAppExist(venerableAppName(appName))
				fatalIf(err)
				if(appExists) {
					fmt.Println("Found old version of app running, deleting.")
					return appRepo.DeleteApplication(venerableAppName(appName))
				} else {
					return nil
				}
			},
		},
		// rename
		{
			Forward: func() error {
				return appRepo.RenameApplication(appName, venerableAppName(appName))
			},
		},
		// push
		{
			Forward: func() error {
				return appRepo.PushApplication(appName, manifestPath, appPath)
			},
			ReversePrevious: func() error {
				// If the app cannot start we'll have a lingering application
				// We delete this application so that the rename can succeed
				appRepo.DeleteApplication(appName)

				return appRepo.RenameApplication(venerableAppName(appName), appName)
			},
		},
		// delete/stop
		{
			Forward: func() error {
				if(options.KeepExisting){
					fmt.Println("Stopping old version of app. Remove the --keep-existing-app flag to delete it automatically.")
					return appRepo.StopApplication(venerableAppName(appName))
				} else {
					fmt.Println("Deleting old version of app. Use the --keep-existing-app flag to preserve it.")
					return appRepo.DeleteApplication(venerableAppName(appName))
				}
			},
		},
	}
}

func getActionsForNewApp(appRepo *ApplicationRepo, appName, manifestPath, appPath string) []rewind.Action {
	return []rewind.Action{
		// push
		{
			Forward: func() error {
				return appRepo.PushApplication(appName, manifestPath, appPath)
			},
		},
	}
}

func (plugin AutopilotPlugin) Run(cliConnection plugin.CliConnection, args []string) {
	appRepo := NewApplicationRepo(cliConnection)

	appName := args[1]
	var actionList []rewind.Action
	var	successMessage string

	if(args[0] == "zero-downtime-push") {
		actionList = getActionsForPush(appRepo, args)
		successMessage = "A new version of your application has successfully been pushed!"
	} else if (args[0] == "zero-downtime-rollback") {
		appExists, err := appRepo.DoesAppExist(appName)
		fatalIf(err)
		venerableAppExists, err := appRepo.DoesAppExist(venerableAppName(appName))
		fatalIf(err)

		if(!appExists){
			fatalIf(errors.New(fmt.Sprintf("Live version of app \"%s\" not found, cannot rollback.", appName)))
		}
		if(!venerableAppExists){
			fatalIf(errors.New(fmt.Sprintf("Venerable version of \"%s\" not found, cannot rollback. Make sure you push with the " +
			"--keep-existing-app flag to leave the venerable version behind.", appName)))
		}
		actionList = getActionsForRollback(appName, appRepo, args)
		successMessage = "Your application has been successfully rolled back!"
	}

	actions := rewind.Actions{
		Actions:              actionList,
		RewindFailureMessage: "Oh no. Something's gone wrong. I've tried to roll back but you should check to see if everything is OK.",
	}

	err := actions.Execute()
	fatalIf(err)

	fmt.Println()
	fmt.Println(successMessage)
	fmt.Println()

	err = appRepo.ListApplications()
	fatalIf(err)
}

func (AutopilotPlugin) GetMetadata() plugin.PluginMetadata {
	return plugin.PluginMetadata{
		Name: "autopilot",
		Version: plugin.VersionType{
			Major: 0,
			Minor: 0,
			Build: 3,
		},
		Commands: []plugin.Command{
			{
				Name:     "zero-downtime-push",
				HelpText: "Perform a zero-downtime push of an application over the top of an old one",
				UsageDetails: plugin.Usage{
					Usage: "$ cf zero-downtime-push application-to-replace \\ \n \t-f path/to/new_manifest.yml \\ \n \t-p path/to/new/path",
				},
			},
			{
				Name:"zero-downtime-rollback",
				HelpText: "Perform a zero-downtime rollback to the previous version of the application. Requires that the previous, 'venerable' version of the app still exists." +
					"Use the --keep-existing-app flag when performing a zero-downtime-push to ensure this.",
				UsageDetails:plugin.Usage{
					Usage:"$cf zero-downtime-rollback application-to-revert",
				},
			},
		},
	}
}

func ParseArgs(args []string) (string, string, string, AutopilotOptions, error) {
	flags := flag.NewFlagSet("zero-downtime-push", flag.ContinueOnError)
	manifestPath := flags.String("f", "", "path to an application manifest")
	appPath := flags.String("p", "", "path to application files")
	keepVenerable := flags.Bool("keep-existing-app", false, "keep existing app running")

	err := flags.Parse(args[2:])
	if err != nil {
		return "", "", "", AutopilotOptions{}, err
	}

	appName := args[1]

	if *manifestPath == "" {
		return "", "", "", AutopilotOptions{}, ErrNoManifest
	}

	options := AutopilotOptions{KeepExisting: *keepVenerable}

	return appName, *manifestPath, *appPath, options, nil
}

var ErrNoManifest = errors.New("a manifest is required to push this application")

type ApplicationRepo struct {
	conn plugin.CliConnection
}

type AutopilotOptions struct {
	KeepExisting bool
}

func NewApplicationRepo(conn plugin.CliConnection) *ApplicationRepo {
	return &ApplicationRepo{
		conn: conn,
	}
}

func (repo *ApplicationRepo) RenameApplication(oldName, newName string) error {
	_, err := repo.conn.CliCommand("rename", oldName, newName)
	return err
}

func (repo *ApplicationRepo) PushApplication(appName, manifestPath, appPath string) error {
	args := []string{"push", appName, "-f", manifestPath}

	if appPath != "" {
		args = append(args, "-p", appPath)
	}

	_, err := repo.conn.CliCommand(args...)
	return err
}

func (repo *ApplicationRepo) DeleteApplication(appName string) error {
	_, err := repo.conn.CliCommand("delete", appName, "-f")
	return err
}

func (repo *ApplicationRepo) StartApplication(appName string) error {
	_, err := repo.conn.CliCommand("start", appName)
	return err
}

func (repo *ApplicationRepo) StopApplication(appName string) error {
	_, err := repo.conn.CliCommand("stop", appName)
	return err
}

func (repo *ApplicationRepo) ListApplications() error {
	_, err := repo.conn.CliCommand("apps")
	return err
}

func (repo *ApplicationRepo) DoesAppExist(appName string) (bool, error) {
	space, err := repo.conn.GetCurrentSpace()
	if err != nil {
		return false, err
	}

	path := fmt.Sprintf(`v2/apps?q=name:%s&q=space_guid:%s`, appName, space.Guid)
	result, err := repo.conn.CliCommandWithoutTerminalOutput("curl", path)

	if err != nil {
		return false, err
	}

	jsonResp := strings.Join(result, "")

	output := make(map[string]interface{})
	err = json.Unmarshal([]byte(jsonResp), &output)

	if err != nil {
		return false, err
	}

	totalResults, ok := output["total_results"]

	if !ok {
		return false, errors.New("Missing total_results from api response")
	}

	count, ok := totalResults.(float64)

	if !ok {
		return false, fmt.Errorf("total_results didn't have a number %v", totalResults)
	}

	return count == 1, nil
}
