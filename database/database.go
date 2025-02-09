package database

import (
	"context"

	database2 "github.com/TicketsBot-cloud/database"
	"github.com/TicketsBot/GoPanel/config"
	"github.com/TicketsBot/database"
	"github.com/jackc/pgconn"
	"github.com/jackc/pgconn/stmtcache"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/log/logrusadapter"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/sirupsen/logrus"
)

var Client *database.Database
var Client2 *database2.Database

func ConnectToDatabase() {
	config, err := pgxpool.ParseConfig(config.Conf.Database.Uri)
	if err != nil {
		panic(err)
	}

	// TODO: Sentry
	config.ConnConfig.LogLevel = pgx.LogLevelWarn
	config.ConnConfig.Logger = logrusadapter.NewLogger(logrus.New())

	config.MinConns = 1
	config.MaxConns = 3

	config.ConnConfig.BuildStatementCache = func(conn *pgconn.PgConn) stmtcache.Cache {
		return stmtcache.New(conn, stmtcache.ModeDescribe, 512)
	}

	pool, err := pgxpool.ConnectConfig(context.Background(), config)
	if err != nil {
		panic(err)
	}

	Client = database.NewDatabase(pool)
	Client2 = database2.NewDatabase(pool)
}
