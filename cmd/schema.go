/* Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.*/

package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/spanner-migration-tool/common/constants"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/common/utils"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/conversion"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/expressions_api"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/internal"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/logger"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/proto/migration"
	"github.com/GoogleCloudPlatform/spanner-migration-tool/sources/common"
	"github.com/google/subcommands"
	"go.uber.org/zap"
)

// SchemaCmd struct with flags.
type SchemaCmd struct {
	source        string
	sourceProfile string
	target        string
	targetProfile string
	filePrefix    string // TODO: move filePrefix to global flags
	project       string
	logLevel      string
	dryRun        bool
	validate      bool
	sessionJSON   string
}

// Name returns the name of operation.
func (cmd *SchemaCmd) Name() string {
	return "schema"
}

// Synopsis returns summary of operation.
func (cmd *SchemaCmd) Synopsis() string {
	return "generate schema for target db from source db schema"
}

// Usage returns usage info of the command.
func (cmd *SchemaCmd) Usage() string {
	return fmt.Sprintf(`%v schema -source=[source] -source-profile="key1=value1,key2=value2" ...

Convert schema for source db specified by source and source-profile. Source db
dump file can be specified by either file param in source-profile or piped to
stdin. Connection profile for source database in direct connect mode can be
specified by setting appropriate params in source-profile. The schema flags are:
`, path.Base(os.Args[0]))
}

// SetFlags sets the flags.
func (cmd *SchemaCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&cmd.source, "source", "", "Flag for specifying source DB, (e.g., `PostgreSQL`, `MySQL`, `DynamoDB`)")
	f.StringVar(&cmd.sourceProfile, "source-profile", "", "Flag for specifying connection profile for source database e.g., \"file=<path>,format=dump\"")
	f.StringVar(&cmd.target, "target", "Spanner", "Specifies the target DB, defaults to Spanner (accepted values: `Spanner`)")
	f.StringVar(&cmd.targetProfile, "target-profile", "", "Flag for specifying connection profile for target database e.g., \"dialect=postgresql\"")
	f.StringVar(&cmd.filePrefix, "prefix", "", "File prefix for generated files")
	f.StringVar(&cmd.project, "project", "", "Flag spcifying default project id for all the generated resources for the migration")
	f.StringVar(&cmd.logLevel, "log-level", "DEBUG", "Configure the logging level for the command (INFO, DEBUG), defaults to DEBUG")
	f.BoolVar(&cmd.dryRun, "dry-run", false, "Flag for generating DDL and schema conversion report without creating a spanner database")
	f.BoolVar(&cmd.validate, "validate", false, "Flag for validating if all the required input parameters are present")
	f.StringVar(&cmd.sessionJSON, "session", "", "Optional. Specifies the file we restore session state from.")
}

func (cmd *SchemaCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	// Cleanup smt tmp data directory in case residuals remain from prev runs.
	os.RemoveAll(filepath.Join(os.TempDir(), constants.SMT_TMP_DIR))
	var err error
	defer func() {
		if err != nil {
			logger.Log.Fatal("FATAL error", zap.Error(err))
		}
	}()
	err = logger.InitializeLogger(cmd.logLevel)
	if err != nil {
		fmt.Println("Error initialising logger, did you specify a valid log-level? [DEBUG, INFO, WARN, ERROR, FATAL]", err)
		return subcommands.ExitFailure
	}
	defer logger.Log.Sync()
	// validate and parse source-profile, target-profile and source
	sourceProfile, targetProfile, ioHelper, dbName, err := PrepareMigrationPrerequisites(cmd.sourceProfile, cmd.targetProfile, cmd.source)
	if err != nil {
		err = fmt.Errorf("error while preparing prerequisites for migration: %v", err)
		return subcommands.ExitUsageError
	}
	if cmd.project == "" {
		getInfo := &utils.GetUtilInfoImpl{}
		cmd.project, err = getInfo.GetProject()
		if err != nil {
			logger.Log.Error("Could not get project id from gcloud environment or --project flag. Either pass the projectId in the --project flag or configure in gcloud CLI using gcloud config set", zap.Error(err))
			return subcommands.ExitUsageError
		}
	}

	if cmd.validate {
		return subcommands.ExitSuccess
	}

	// If filePrefix not explicitly set, use generated dbName.
	if cmd.filePrefix == "" {
		cmd.filePrefix = dbName
	}

	schemaConversionStartTime := time.Now()
	var conv *internal.Conv
	convImpl := &conversion.ConvImpl{}
	if cmd.sessionJSON != "" {
		logger.Log.Info("Loading the conversion context from session file."+
			" The source profile will not be used for the schema conversion.", zap.String("sessionFile", cmd.sessionJSON))
		conv = internal.MakeConv()
		err = conversion.ReadSessionFile(conv, cmd.sessionJSON)
		if err != nil {
			return subcommands.ExitFailure
		}
		expressionVerificationAccessor, _ := expressions_api.NewExpressionVerificationAccessorImpl(context.Background(), targetProfile.Conn.Sp.Project, targetProfile.Conn.Sp.Instance)
		schemaToSpanner := common.SchemaToSpannerImpl{
			ExpressionVerificationAccessor: expressionVerificationAccessor,
		}
		err := schemaToSpanner.VerifyExpressions(conv)

		if err != nil {
			logger.Log.Error(fmt.Sprintf("Error while verifying the expressions %v", err))
			return subcommands.ExitFailure
		}
	} else {
		ctx := context.Background()
		ddlVerifier, err := expressions_api.NewDDLVerifierImpl(ctx, "", "")
		if err != nil {
			logger.Log.Error(fmt.Sprintf("error trying create ddl verifier: %v", err))
			return subcommands.ExitFailure
		}
		sfs := &conversion.SchemaFromSourceImpl{
			DdlVerifier: ddlVerifier,
		}
		conv, err = convImpl.SchemaConv(cmd.project, sourceProfile, targetProfile, &ioHelper, sfs)
		if err != nil {
			return subcommands.ExitFailure
		}
	}
	if conv == nil {
		logger.Log.Error("Could not initialize conversion context from")
		return subcommands.ExitFailure
	}
	conversion.WriteSchemaFile(conv, schemaConversionStartTime, cmd.filePrefix+schemaFile, ioHelper.Out, sourceProfile.Driver)
	// We always write the session file to accommodate for a re-run that might change anything.
	conversion.WriteSessionFile(conv, cmd.filePrefix+sessionFile, ioHelper.Out)

	// Populate migration request id and migration type in conv object.
	conv.Audit.MigrationRequestId, _ = utils.GenerateName("smt-job")
	conv.Audit.MigrationRequestId = strings.Replace(conv.Audit.MigrationRequestId, "_", "-", -1)
	conv.Audit.MigrationType = migration.MigrationData_SCHEMA_ONLY.Enum()
	conv.Audit.SkipMetricsPopulation = os.Getenv("SKIP_METRICS_POPULATION") == "true"
	if !cmd.dryRun {
		_, err = MigrateDatabase(ctx, cmd.project, targetProfile, sourceProfile, dbName, &ioHelper, cmd, conv, nil)
		if err != nil {
			err = fmt.Errorf("can't finish database migration for db %s: %v", dbName, err)
			return subcommands.ExitFailure
		}
	}

	schemaCoversionEndTime := time.Now()
	conv.Audit.SchemaConversionDuration = schemaCoversionEndTime.Sub(schemaConversionStartTime)
	banner := utils.GetBanner(schemaConversionStartTime, dbName)
	reportImpl := conversion.ReportImpl{}
	reportImpl.GenerateReport(sourceProfile.Driver, nil, ioHelper.BytesRead, banner, conv, cmd.filePrefix, dbName, ioHelper.Out)
	// Cleanup smt tmp data directory.
	os.RemoveAll(filepath.Join(os.TempDir(), constants.SMT_TMP_DIR))
	return subcommands.ExitSuccess
}
