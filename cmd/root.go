// Copyright © 2018 Humio Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"
	"os"
	"path"

	"github.com/humio/cli/api"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string
var token string
var address string

// rootCmd represents the base command when called without any subcommands
var rootCmd *cobra.Command

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	rootCmd = &cobra.Command{
		Use:   "humio [subcommand] [flags] [arguments]",
		Short: "A management CLI for Humio.",
		Long: `To set up your environment run:

  $ humio login

Sending Data:
  Humio's CLI is not a replacement for fully-featured data-shippers like
	LogStash, FileBeat or MetricBeat. It can be handy to easily send logs
	to Humio to, e.g examine a local log file or test a parser on test input.

To stream the content of "/var/log/system.log" data to Humio:

  $ tail -f /var/log/system.log | humio ingest -o

or

  $ humio ingest -o --tail=/var/log/system.log

Common commands:
  users <subcommand>
  parsers <subcommand>
		`,
	}

	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file (default is $HOME/.humio/config.yaml)")
	rootCmd.PersistentFlags().StringVarP(&token, "token", "t", "", "The API token to user when talking to Humio. Overrides the value in your config file.")
	rootCmd.PersistentFlags().StringVarP(&address, "address", "a", "http://localhost:8080/", "The HTTP address of the Humio cluster. Overrides the value in your config file. (default http://localhost:8080/)")

	viper.BindPFlag("address", rootCmd.PersistentFlags().Lookup("address"))
	viper.BindPFlag("token", rootCmd.PersistentFlags().Lookup("token"))

	rootCmd.AddCommand(newUsersCmd())
	rootCmd.AddCommand(newParsersCmd())
	rootCmd.AddCommand(newIngestCmd())
	rootCmd.AddCommand(newLoginCmd())
	rootCmd.AddCommand(newIngestTokensCmd())
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		cfgFile = path.Join(home, ".humio", "config.yaml")
		viper.SetConfigFile(cfgFile)
		viper.SetConfigType("yaml")
	}

	viper.SetEnvPrefix("HUMIO")
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	err := viper.ReadInConfig()
	if err == nil {
		fmt.Println("Cluster Address:", viper.Get("address"))
	} else {
		fmt.Println(err)
	}

	fmt.Println("Using config file:", cfgFile)
}

func NewApiClient(cmd *cobra.Command) *api.Client {
	config := api.DefaultConfig()
	config.Address = viper.GetString("address")
	config.Token = viper.GetString("token")

	client, err := api.NewClient(config)

	if err != nil {
		fmt.Println(fmt.Errorf("Error creating HTTP client: %s", err))
		os.Exit(1)
	}

	return client
}
