package server

import (
	"github.com/artpar/api2go"
	"github.com/artpar/api2go-adapter/gingonic"
	log "github.com/sirupsen/logrus"
	"gopkg.in/gin-gonic/gin.v1"
	"github.com/artpar/goms/server/auth"
	"github.com/artpar/goms/server/resource"
	"github.com/jamiealquiza/envy"
	"net/http"
	"fmt"
	"io/ioutil"
	"flag"
	"github.com/artpar/rclone/fs"
	"github.com/satori/go.uuid"
)

var cruds = make(map[string]*resource.DbResource)

func Main(boxRoot, boxStatic http.FileSystem) {

	var port = flag.String("port", "6336", "GoMS port")
	var db_type = flag.String("db_type", "sqlite3", "Database to use: sqlite3/mysql/postgres")
	var connection_string = flag.String("db_connection_string", "test.db", "\n\tSQLite: test.db\n"+
			"\tMySql: <username>:<password>@tcp(<hostname>:<port>)/<db_name>\n"+
			"\tPostgres: host=<hostname> port=<port> user=<username> password=<password> dbname=<db_name> sslmode=enable/disable")

	var runtimeMode = flag.String("runtime", "debug", "Runtime for Gin: debug, test, release")

	envy.Parse("GOMS") // looks for GOMS_PORT
	flag.Parse()

	gin.SetMode(*runtimeMode)

	//configFile := "gocms_style.json"

	db, err := GetDbConnection(*db_type, *connection_string)
	if err != nil {
		panic(err)
	}

	/// Start system initialise

	log.Infof("Load config files")
	initConfig, errs := loadConfigFiles()
	if errs != nil {
		for _, err := range errs {
			log.Errorf("Failed to load config file: %v", err)
		}
	}

	existingTables, _ := GetTablesFromWorld(db)
	//initConfig.Tables = append(initConfig.Tables, existingTables...)
	existingTablesMap := make(map[string]bool)

	allTables := make([]resource.TableInfo, 0)

	for j, existableTable := range existingTables {
		existingTablesMap[existableTable.TableName] = true
		var isBeingModified = false
		var indexBeingModified = -1

		for i, newTable := range initConfig.Tables {
			if newTable.TableName == existableTable.TableName {
				isBeingModified = true
				indexBeingModified = i
				break
			}
		}

		if isBeingModified {
			log.Infof("Table %s is being modified", existableTable.TableName)
			tableBeingModified := initConfig.Tables[indexBeingModified]

			if len(tableBeingModified.Columns) > 0 {

				for _, newColumnDef := range tableBeingModified.Columns {
					columnAlreadyExist := false
					for _, existingColumn := range existableTable.Columns {
						if existingColumn.ColumnName == newColumnDef.ColumnName {
							columnAlreadyExist = true
							break
						}
					}
					if columnAlreadyExist {
						log.Infof("Modifying existing columns[%v][%v] is not supported at present. not sure what would break. and alter query isnt being run currently.", existableTable.TableName, newColumnDef.Name);
					} else {
						existableTable.Columns = append(existableTable.Columns, newColumnDef)
					}

				}

			}
			if len(tableBeingModified.Relations) > 0 {
				existableTable.Relations = append(existableTable.Relations, tableBeingModified.Relations...)
			}
			existingTables[j] = existableTable
		}
		allTables = append(allTables, existableTable)
	}

	for _, newTable := range initConfig.Tables {
		if existingTablesMap[newTable.TableName] {
			continue
		}
		allTables = append(allTables, newTable)

	}
	initConfig.Tables = allTables
	fs.LoadConfig()
	fs.Config.DryRun = false
	fs.Config.LogLevel = 200
	fs.Config.StatsLogLevel = 200

	resource.CheckRelations(&initConfig, db)
	resource.CheckAuditTables(&initConfig, db)

	//AddStateMachines(&initConfig, db)

	resource.CheckAllTableStatus(&initConfig, db)

	resource.CreateRelations(&initConfig, db)

	resource.CreateUniqueConstraints(&initConfig, db)
	resource.CreateIndexes(&initConfig, db)

	resource.UpdateWorldTable(&initConfig, db)
	resource.UpdateWorldColumnTable(&initConfig, db)
	resource.UpdateStateMachineDescriptions(&initConfig, db)
	resource.UpdateExchanges(&initConfig, db)

	err = resource.UpdateActionTable(&initConfig, db)
	resource.CheckErr(err, "Failed to update action table")

	//CleanUpConfigFiles()

	/// end system initialise

	r := gin.Default()
	r.Use(CorsMiddlewareFunc)
	r.StaticFS("/static", boxStatic)

	r.GET("/favicon.ico", func(c *gin.Context) {

		file, err := boxRoot.Open("index.html")
		fileContents, err := ioutil.ReadAll(file)
		_, err = c.Writer.Write(fileContents)
		resource.CheckErr(err, "Failed to write favico")
	})

	configStore, err := resource.NewConfigStore(db)
	jwtSecret, err := configStore.GetConfigValueFor("jwt.secret", "backend")

	if err != nil {
		newSecret := uuid.NewV4().String()
		configStore.SetConfigValueFor("jwt.secret", newSecret, "backend")
		jwtSecret = newSecret
	}

	resource.CheckError(err, "Failed to get config store")
	err = CheckSystemSecrets(configStore)
	resource.CheckErr(err, "Failed to initialise system secrets")

	r.GET("/config", CreateConfigHandler(configStore))

	authMiddleware := auth.NewAuthMiddlewareBuilder(db)
	auth.InitJwtMiddleware([]byte(jwtSecret))
	r.Use(authMiddleware.AuthCheckMiddleware)

	r.GET("/actions", resource.CreateGuestActionListHandler(&initConfig, cruds))

	api := api2go.NewAPIWithRouting(
		"api",
		api2go.NewStaticResolver("/"),
		gingonic.New(r),
	)

	ms := BuildMiddlewareSet(&initConfig)
	cruds = AddResourcesToApi2Go(api, initConfig.Tables, db, &ms, configStore)
	hostSwitch := CreateSubSites(&initConfig, db, cruds)

	hostSwitch.handlerMap["default"] = r
	go resource.ImportDataFiles(&initConfig, db, cruds)

	authMiddleware.SetUserCrud(cruds["user"])
	authMiddleware.SetUserGroupCrud(cruds["usergroup"])
	authMiddleware.SetUserUserGroupCrud(cruds["user_user_id_has_usergroup_usergroup_id"])

	fsmManager := resource.NewFsmManager(db, cruds)

	r.GET("/ping", func(c *gin.Context) {
		c.String(200, "pong")
	})

	r.GET("/jsmodel/:typename", CreateJsModelHandler(&initConfig))
	r.GET("/apiblueprint.json", CreateApiBlueprintHandler(&initConfig, cruds))
	r.OPTIONS("/jsmodel/:typename", CreateJsModelHandler(&initConfig))

	actionPerformers := GetActionPerformers(&initConfig, configStore)

	r.POST("/action/:typename/:actionName", resource.CreatePostActionHandler(&initConfig, configStore, cruds, actionPerformers))
	r.GET("/action/:typename/:actionName", resource.CreatePostActionHandler(&initConfig, configStore, cruds, actionPerformers))

	r.POST("/track/start/:stateMachineId", CreateEventStartHandler(fsmManager, cruds, db))
	r.POST("/track/event/:typename/:objectStateId/:eventName", CreateEventHandler(&initConfig, fsmManager, cruds, db))

	r.GET("/site/content", CreateSubSiteContentHandler(&initConfig, cruds, db))
	r.POST("/site/content", CreateSubSiteSaveContentHandler(&initConfig, cruds, db))

	r.NoRoute(func(c *gin.Context) {
		file, err := boxRoot.Open("index.html")
		fileContents, err := ioutil.ReadAll(file)
		_, err = c.Writer.Write(fileContents)
		resource.CheckErr(err, "Failed to write index html")
	})

	resource.InitialiseColumnManager()

	//r.Run(fmt.Sprintf(":%v", *port))

	http.ListenAndServe(fmt.Sprintf(":%v", *port), hostSwitch)
}
