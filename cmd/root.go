package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/fatih/color"
	"github.com/hokaccha/go-prettyjson"
	"gopkg.in/yaml.v2"

	"github.com/sirupsen/logrus"

	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// VersionInfo is used by the `--version` command to output version info.
type VersionInfo struct {
	Version string
	Commit  string
	Date    string
	BuiltBy string
}

var vInfo VersionInfo

func (v VersionInfo) String() string {
	return fmt.Sprintf(
		"ecsrun version info\nVersion: %s\nCommit: %s\nDate Built: %s\nBuilt By: %s\n",
		v.Version,
		v.Commit,
		v.Date,
		v.BuiltBy)
}

var log = logrus.New()

var fs = afero.NewOsFs()

var newEcsClient func(*RunConfig) ECSClient

var rootCmd *cobra.Command = &cobra.Command{
	Use:   "escrun",
	Short: "Easily run one-off tasks against an ECS Cluster",
	Long: `
ecsrun is a CLI tool that allows users to run one-off administrative tasks
using their existing Task Definitions.`,

	Run: func(cmd *cobra.Command, args []string) {
		if err := initConfigFile(); err != nil {
			log.Debug(err)
		}

		// Raise and exit if we're missing any required flags
		if err := checkRequired(); err != nil {
			fmt.Print(err.Error())
			os.Exit(1)
		}

		config := BuildRunConfig()
		ecsClient := newEcsClient(config)

		input := ecsClient.BuildRunTaskInput()

		// Oooh fancy.
		prettyBytes, _ := prettyjson.Marshal(input)
		prettyString := string(prettyBytes)

		// If we're running with --dry-run then print the input and exit.
		if viper.GetBool("dry-run") {
			cyan := color.New(color.FgCyan, color.Bold)
			cyan.Printf("DryRun! RunTaskInput:\n")

			fmt.Println(prettyString)

			os.Exit(0)
		}

		log.Debug("RunTaskInput: ", prettyString)
		output, err := ecsClient.RunTask(input)
		if err != nil {
			log.Fatal(err)
		}

		prettyOut, _ := prettyjson.Marshal(output)
		log.Info("RunTaskOutput: ", string(prettyOut))
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute(n func(*RunConfig) ECSClient, v VersionInfo) {
	newEcsClient = n
	vInfo = v
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func init() {
	cobra.OnInitialize(initEnvVars, initVerbose, initVersion, initAws)

	log.SetOutput(os.Stderr)

	// Basic Flags
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().Bool("version", false, "version output")

	// Config File Flags
	rootCmd.PersistentFlags().String("config-file", "", "config file to read config entries from (default is $PWD/escrun.yml)")
	rootCmd.PersistentFlags().String("config", "default", "config entry to read in the config file (default is 'default')")
	rootCmd.PersistentFlags().Bool("dry-run", false, "dry-run your ecsrun execution to check config (default is false)")

	// AWS Cred / Environment Flags
	rootCmd.PersistentFlags().String("cred", "", "AWS credentials file (default is $HOME/.aws/.credentials)")
	rootCmd.PersistentFlags().StringP("profile", "p", "", "AWS profile to target (default is AWS_PROFILE or 'default')")
	rootCmd.PersistentFlags().String("region", "", `AWS region to target (default is AWS_REGION or pulled from $HOME/.aws/.credentials)`)

	// Task Flags
	rootCmd.PersistentFlags().StringP("cluster", "c", "", "The ECS Cluster to run the task in.")
	rootCmd.PersistentFlags().StringP("task", "t", "", "The name of the ECS Task Definition to use.")
	rootCmd.PersistentFlags().StringP("revision", "r", "", "The Task Definition revision to use.")
	rootCmd.PersistentFlags().StringP("name", "n", "", "The name of the container in the Task Definition.")
	rootCmd.PersistentFlags().StringP("launch-type", "l", "FARGATE", "The launch type to run as. Currently only Fargate is supported.")
	rootCmd.PersistentFlags().StringSlice("cmd", []string{}, "The comma separated command override to apply.")
	rootCmd.PersistentFlags().Int64("count", 1, "The number of tasks to launch for the given cmd.")

	// Network Flags
	rootCmd.PersistentFlags().StringP("subnet", "s", "", "The Subnet ID that the task should be launched in.")
	rootCmd.PersistentFlags().StringP("security-group", "g", "", "The Security Group ID that the task should be associated with.")
	rootCmd.PersistentFlags().Bool("public", false, "Assigns a public IP to the task if included. (default is false)")

	viper.BindPFlags(rootCmd.PersistentFlags())
}

func initEnvVars() {
	viper.SetEnvPrefix("ecsrun")

	// Bind Vars to Env Variables
	viper.BindEnv("verbose")
	viper.BindEnv("cluster")
	viper.BindEnv("task")
	viper.BindEnv("cmd")
	viper.BindEnv("subnet")
	viper.BindEnv("security-group", "ECSRUN_SECURITY_GROUP")

	// read in environment variables that match the above
	viper.AutomaticEnv()
}

func initVerbose() {
	if viper.GetBool("verbose") {
		log.Info("Enabling verbose output.")
		log.SetLevel(logrus.DebugLevel)
	}
}

func initVersion() {
	if viper.GetBool("version") {
		fmt.Print(vInfo.String())
		os.Exit(0)
	}
}

func initAws() {
	profile := getProfile()
	viper.Set("profile", profile)

	// Create our AWS session object for AWS API Usage
	sesh, err := initAwsSession(profile)
	if err != nil {
		log.Fatal("Unable to init AWS Session. Check your credentials and profile.", err)
	}

	region := viper.GetString("region")
	if region == "" {
		region = *sesh.Config.Region
	}

	// Override our Session's region in case it was set.
	sesh.Config.WithRegion(region)

	// Set our awsSession for later use.
	viper.Set("session", sesh)
}

func getProfile() string {
	var profile = viper.GetString("profile")
	if profile == "" {
		profile = "default"
		if os.Getenv("AWS_PROFILE") != "" {
			profile = os.Getenv("AWS_PROFILE")
		}
	}

	return profile
}

func initAwsSession(profile string) (*session.Session, error) {
	credFile, err := rootCmd.PersistentFlags().GetString("cred")
	if err != nil {
		log.Fatal("Not able to get credFile from cmd.", err)
	}

	var sesh *session.Session

	if credFile != "" {
		sesh, err = session.NewSession(&aws.Config{
			Credentials: credentials.NewSharedCredentials(credFile, profile),
		})
	} else {
		sesh = session.Must(session.NewSessionWithOptions(session.Options{
			Profile:           profile,
			SharedConfigState: session.SharedConfigEnable,
			Config: aws.Config{
				CredentialsChainVerboseErrors: aws.Bool(true),
			},
		}))
	}

	return sesh, err
}

func initConfigFile() error {
	var filename string
	var err error

	cfgFile := viper.GetString("config-file")

	if cfgFile == "" {
		filename, err = findConfigFile()
	} else {
		filename, err = findCustomConfigFile(cfgFile)
	}

	if err != nil {
		return err
	}

	log.Debug("Using config file: ", filename)

	file, err := afero.ReadFile(fs, filename)
	if err != nil {
		return err
	}

	config := make(map[string]map[string]interface{})
	if err := yaml.Unmarshal(file, &config); err != nil {
		return err
	}

	log.Debug("Full config file contents: ", config)

	configEntry := viper.GetString("config")

	log.Debug("Config entry: ", configEntry, " result: ", config[configEntry])
	if err = viper.MergeConfigMap(config[configEntry]); err != nil {
		return err
	}

	return nil
}

func findCustomConfigFile(filename string) (string, error) {
	log.Info("filename: ", filename)
	exists, err := afero.Exists(fs, filename)
	if err != nil {
		return "", err
	}

	if exists {
		return filename, nil
	}

	return "", errors.New("custom config file not found")
}

func findConfigFile() (string, error) {
	supportedExts := []string{"yaml", "yml"}

	for _, extension := range supportedExts {
		filename := filepath.Join(".", "ecsrun"+"."+extension)
		exists, err := afero.Exists(fs, filename)
		if err != nil {
			return "", err
		}

		if exists {
			return filename, nil
		}
	}
	return "", errors.New("config file not found")
}

// checkRequired maps over all the required flags and creates a nice err msg if
// any are found. This is used instead of Cobra native required flags due to
// the goofy configuration file setup.
func checkRequired() error {
	requiredFlags := []string{"cluster", "task", "cmd", "subnet", "security-group"}
	unsetFlags := []string{}
	for _, flag := range requiredFlags {
		if !viper.IsSet(flag) {
			unsetFlags = append(unsetFlags, flag)
		}
	}

	if len(unsetFlags) > 0 {
		red := color.New(color.FgRed)
		redB := color.New(color.FgRed, color.Bold)

		start := red.Sprintf("The following are required arguments: ")
		reqArgs := redB.Sprintf("%s", strings.Join(unsetFlags, ", "))

		errMsg := fmt.Sprintf("%s%s\n", start, reqArgs)
		return errors.New(errMsg)
	}

	return nil
}
