package gorm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/aws/aws-xray-sdk-go/xray"
	"github.com/sirupsen/logrus"
	"reflect"
	"strings"
	"sync"
	"time"
)

type ctxDB struct {
	dbSQL      SQLCommon //主库，写或事务操作
	dbSQLSlave SQLCommon //从库，非事务读操作
	ctx        context.Context
	source     string
}

//用在query中，如果是事务或是写操作用主库，否则用从库
func (db ctxDB) getDBSQLInNoTxQuery() (dbSQL SQLCommon) {
	dbSQL = db.dbSQL
	if _, ok := dbSQL.(*sql.Tx); !ok { //不是事务才用读库
		if db.dbSQLSlave != nil { //从库存在才用从库，否则还是用主库
			dbSQL = db.dbSQLSlave
		}
	}
	return
}

//明确表示使用主库:
// 由于上面的getDBSQLInNoTxQuery方法在取不到dbSQLSlave时候会使用主库，
// 所以这里简单起见，把dbSQLSlave置nil，
// 如果没有主库，那么后面执行sql时候会报空指针的错误，符合逻辑
func (db *ctxDB) useMaster() {
	db.dbSQLSlave = nil
}

//为了记录trace_id而直接打日志
func beginSeg(db ctxDB, query string, args ...interface{}) func(errPtr *error, r func() *int64) {
	sql := PrintSQL(query, args...)
	entry := logrus.WithContext(db.ctx).WithFields(logrus.Fields{
		"sql":    sql,
		"stack":  nil,
		"source": db.source,
	})
	start := time.Now()
	var seg *xray.Segment
	if db.ctx != nil && xray.GetSegment(db.ctx) != nil {
		_, seg = xray.BeginSubsegment(db.ctx, db.source)
		seg.Namespace = "remote"
		seg.GetSQL().SanitizedQuery = sql
	}
	return func(errPtr *error, getRows func() *int64) {
		var err error
		if errPtr != nil {
			err = *errPtr
		}
		end := time.Now()
		if seg != nil {
			seg.Close(err)
		}
		duration := end.Sub(start)

		entry = entry.WithField("duration", duration.String())
		if r := getRows(); r != nil {
			entry = entry.WithField("exec_rows", *r) //只打印执行语句的行数，不打印查询语句行数
		}
		if err != nil {
			entry.WithError(err).Error()
			return
		}
		if duration >= 200*time.Millisecond {
			entry.Warn("slow sql") //慢查询警告
			return
		}
		entry.Debug()
		if db.ctx == nil {
			entry.Trace("nil context, forget call WithContext?") //不然比较吵人
			return
		}
	}
}

var rowsNil = func() *int64 { return nil }

func (db ctxDB) Exec(query string, args ...interface{}) (result sql.Result, err error) {
	defer beginSeg(db, query, args...)(&err, func() *int64 {
		if err != nil {
			return nil
		}
		rows, _ := result.RowsAffected()
		return &rows
	})
	result, err = db.dbSQL.Exec(query, args...) //FIXME: 是否需要替换成ExecContent
	return
}
func (db ctxDB) Prepare(query string) (stmt *sql.Stmt, err error) {
	defer beginSeg(db, query)(&err, rowsNil)
	stmt, err = db.dbSQL.Prepare(query)
	return
}
func (db ctxDB) Query(query string, args ...interface{}) (rows *sql.Rows, err error) {
	//NOTE: 不能用rows.Next()来获取长度，因为外面会用rows.Next()把数据拷贝出来，因此不打印行数了
	defer beginSeg(db, query, args...)(&err, rowsNil)
	rows, err = db.getDBSQLInNoTxQuery().Query(query, args...)
	return
}
func (db ctxDB) QueryRow(query string, args ...interface{}) (row *sql.Row) {
	defer beginSeg(db, query, args...)(nil, rowsNil)
	row = db.getDBSQLInNoTxQuery().QueryRow(query, args...)
	return
}

// DB contains information for current db connection
type DB struct {
	sync.RWMutex
	Value        interface{}
	Error        error
	RowsAffected int64

	// interface改成struct
	db                ctxDB
	blockGlobalUpdate bool
	logMode           logModeValue
	logger            logger
	search            *search
	values            sync.Map

	// global db
	parent        *DB
	callbacks     *Callback
	dialect       Dialect
	singularTable bool

	// function to be used to override the creating of a new timestamp
	nowFuncOverride func() time.Time
}

type logModeValue int

const (
	defaultLogMode logModeValue = iota
	noLogMode
	detailedLogMode
)

func (s *DB) WithContext(ctx context.Context) *DB {
	if ctx == nil {
		panic("nil context")
		return s
	}
	clone := s.clone() //NOTE: 复制避免多个线程使用同一个ctx
	clone.db.ctx = ctx
	clone.db.source = GetSource(2)
	return clone
}

// Open initialize a new db connection, need to import driver first, e.g:
//
//     import _ "github.com/go-sql-driver/mysql"
//     func main() {
//       db, err := gorm.Open("mysql", "user:password@/dbname?charset=utf8&parseTime=True&loc=Local")
//     }
// GORM has wrapped some drivers, for easier to remember driver's import path, so you could import the mysql driver with
//    import _ "github.com/lun-zhang/gorm/dialects/mysql"
//    // import _ "github.com/lun-zhang/gorm/dialects/postgres"
//    // import _ "github.com/lun-zhang/gorm/dialects/sqlite"
//    // import _ "github.com/lun-zhang/gorm/dialects/mssql"
func Open(dialect string, args ...interface{}) (db *DB, err error) {
	if len(args) == 0 {
		err = errors.New("invalid database source")
		return nil, err
	}
	var source string
	var dbSQL SQLCommon
	var ownDbSQL bool

	switch value := args[0].(type) {
	case string:
		var driver = dialect
		if len(args) == 1 {
			source = value
		} else if len(args) >= 2 {
			driver = value
			source = args[1].(string)
		}
		dbSQL, err = sql.Open(driver, source)
		ownDbSQL = true
	case SQLCommon:
		dbSQL = value
		ownDbSQL = false
	default:
		return nil, fmt.Errorf("invalid database source: %v is not a valid type", value)
	}

	db = &DB{
		db:        ctxDB{dbSQL: dbSQL},
		logger:    defaultLogger,
		callbacks: DefaultCallback,
		dialect:   newDialect(dialect, dbSQL),
	}
	db.parent = db
	if err != nil {
		return
	}
	// Send a ping to make sure the database connection is alive.
	if d, ok := dbSQL.(*sql.DB); ok {
		if err = d.Ping(); err != nil && ownDbSQL {
			d.Close()
		}
	}
	return
}

func openAndPing(driver, source string) (db *sql.DB, err error) {
	db, err = sql.Open(driver, source)
	if err != nil {
		return
	}
	// Send a ping to make sure the database connection is alive.
	if err = db.Ping(); err != nil {
		db.Close()
	}
	return
}

func OpenMasterAndSlave(driver, master, slave string) (db *DB, err error) {
	var ctxDB ctxDB

	ctxDB.dbSQL, err = openAndPing(driver, master)
	if err != nil {
		return
	}

	ctxDB.dbSQLSlave, err = openAndPing(driver, slave)
	if err != nil {
		return
	}

	db = &DB{
		db:        ctxDB,
		logger:    defaultLogger,
		callbacks: DefaultCallback,
		dialect:   newDialect(driver, ctxDB), //NOTE: dialect也同时使用主库和从库
	}
	db.parent = db
	return
}

// New clone a new db connection without search conditions
func (s *DB) New() *DB {
	clone := s.clone()
	clone.search = nil
	clone.Value = nil
	return clone
}

type closer interface {
	Close() error
}

// Close close current db connection.  If database connection is not an io.Closer, returns an error.
func (s *DB) Close() error {
	if db, ok := s.parent.db.dbSQL.(closer); ok {
		return db.Close()
	}
	return errors.New("can't close current db")
}

//NOTE: 返回的是主库
// DB get `*sql.DB` from current connection
// If the underlying database connection is not a *sql.DB, returns nil
func (s *DB) DB() *sql.DB {
	db, ok := s.db.dbSQL.(*sql.DB)
	if !ok {
		panic("can't support full GORM on currently status, maybe this is a TX instance.")
	}
	return db
}

//返回从库
func (s *DB) DBSlave() *sql.DB {
	db, _ := s.db.dbSQLSlave.(*sql.DB)
	return db
}

//明确表示使用主库:
// 由于从库和主库有几毫秒的延迟，
// 所以写主库，然后立刻读从库这一行时候，可能未读到修改（如果用事务读，就读的是主库，没这个问题），
// 因此增加这个Master方法
func (s *DB) Master() *DB {
	clone := s.clone()
	clone.db.useMaster()
	return clone
}

// CommonDB return the underlying `*sql.DB` or `*sql.Tx` instance, mainly intended to allow coexistence with legacy non-GORM code.
func (s *DB) CommonDB() SQLCommon {
	return s.db.dbSQL
}

// Dialect get dialect
func (s *DB) Dialect() Dialect {
	return s.dialect
}

// Callback return `Callbacks` container, you could add/change/delete callbacks with it
//     db.Callback().Create().Register("update_created_at", updateCreated)
// Refer https://jinzhu.github.io/gorm/development.html#callbacks
func (s *DB) Callback() *Callback {
	s.parent.callbacks = s.parent.callbacks.clone(s.logger)
	return s.parent.callbacks
}

// SetLogger replace default logger
func (s *DB) SetLogger(log logger) {
	s.logger = log
}

// LogMode set log mode, `true` for detailed logs, `false` for no log, default, will only print error logs
func (s *DB) LogMode(enable bool) *DB {
	if enable {
		s.logMode = detailedLogMode
	} else {
		s.logMode = noLogMode
	}
	return s
}

// SetNowFuncOverride set the function to be used when creating a new timestamp
func (s *DB) SetNowFuncOverride(nowFuncOverride func() time.Time) *DB {
	s.nowFuncOverride = nowFuncOverride
	return s
}

// Get a new timestamp, using the provided nowFuncOverride on the DB instance if set,
// otherwise defaults to the global NowFunc()
func (s *DB) nowFunc() time.Time {
	if s.nowFuncOverride != nil {
		return s.nowFuncOverride()
	}

	return NowFunc()
}

// BlockGlobalUpdate if true, generates an error on update/delete without where clause.
// This is to prevent eventual error with empty objects updates/deletions
func (s *DB) BlockGlobalUpdate(enable bool) *DB {
	s.blockGlobalUpdate = enable
	return s
}

// HasBlockGlobalUpdate return state of block
func (s *DB) HasBlockGlobalUpdate() bool {
	return s.blockGlobalUpdate
}

// SingularTable use singular table by default
func (s *DB) SingularTable(enable bool) {
	s.parent.Lock()
	defer s.parent.Unlock()
	s.parent.singularTable = enable
}

// NewScope create a scope for current operation
func (s *DB) NewScope(value interface{}) *Scope {
	dbClone := s.clone()
	dbClone.Value = value
	scope := &Scope{db: dbClone, Value: value}
	if s.search != nil {
		scope.Search = s.search.clone()
	} else {
		scope.Search = &search{}
	}
	return scope
}

// QueryExpr returns the query as SqlExpr object
func (s *DB) QueryExpr() *SqlExpr {
	scope := s.NewScope(s.Value)
	scope.InstanceSet("skip_bindvar", true)
	scope.prepareQuerySQL()

	return Expr(scope.SQL, scope.SQLVars...)
}

// SubQuery returns the query as sub query
func (s *DB) SubQuery() *SqlExpr {
	scope := s.NewScope(s.Value)
	scope.InstanceSet("skip_bindvar", true)
	scope.prepareQuerySQL()

	return Expr(fmt.Sprintf("(%v)", scope.SQL), scope.SQLVars...)
}

// Where return a new relation, filter records with given conditions, accepts `map`, `struct` or `string` as conditions, refer http://jinzhu.github.io/gorm/crud.html#query
func (s *DB) Where(query interface{}, args ...interface{}) *DB {
	return s.clone().search.Where(query, args...).db
}

// Or filter records that match before conditions or this one, similar to `Where`
func (s *DB) Or(query interface{}, args ...interface{}) *DB {
	return s.clone().search.Or(query, args...).db
}

// Not filter records that don't match current conditions, similar to `Where`
func (s *DB) Not(query interface{}, args ...interface{}) *DB {
	return s.clone().search.Not(query, args...).db
}

// Limit specify the number of records to be retrieved
func (s *DB) Limit(limit interface{}) *DB {
	return s.clone().search.Limit(limit).db
}

// Offset specify the number of records to skip before starting to return the records
func (s *DB) Offset(offset interface{}) *DB {
	return s.clone().search.Offset(offset).db
}

// Order specify order when retrieve records from database, set reorder to `true` to overwrite defined conditions
//     db.Order("name DESC")
//     db.Order("name DESC", true) // reorder
//     db.Order(gorm.Expr("name = ? DESC", "first")) // sql expression
func (s *DB) Order(value interface{}, reorder ...bool) *DB {
	return s.clone().search.Order(value, reorder...).db
}

// Select specify fields that you want to retrieve from database when querying, by default, will select all fields;
// When creating/updating, specify fields that you want to save to database
func (s *DB) Select(query interface{}, args ...interface{}) *DB {
	return s.clone().search.Select(query, args...).db
}

// Omit specify fields that you want to ignore when saving to database for creating, updating
func (s *DB) Omit(columns ...string) *DB {
	return s.clone().search.Omit(columns...).db
}

// Group specify the group method on the find
func (s *DB) Group(query string) *DB {
	return s.clone().search.Group(query).db
}

// Having specify HAVING conditions for GROUP BY
func (s *DB) Having(query interface{}, values ...interface{}) *DB {
	return s.clone().search.Having(query, values...).db
}

// Joins specify Joins conditions
//     db.Joins("JOIN emails ON emails.user_id = users.id AND emails.email = ?", "jinzhu@example.org").Find(&user)
func (s *DB) Joins(query string, args ...interface{}) *DB {
	return s.clone().search.Joins(query, args...).db
}

// Scopes pass current database connection to arguments `func(*DB) *DB`, which could be used to add conditions dynamically
//     func AmountGreaterThan1000(db *gorm.DB) *gorm.DB {
//         return db.Where("amount > ?", 1000)
//     }
//
//     func OrderStatus(status []string) func (db *gorm.DB) *gorm.DB {
//         return func (db *gorm.DB) *gorm.DB {
//             return db.Scopes(AmountGreaterThan1000).Where("status in (?)", status)
//         }
//     }
//
//     db.Scopes(AmountGreaterThan1000, OrderStatus([]string{"paid", "shipped"})).Find(&orders)
// Refer https://jinzhu.github.io/gorm/crud.html#scopes
func (s *DB) Scopes(funcs ...func(*DB) *DB) *DB {
	for _, f := range funcs {
		s = f(s)
	}
	return s
}

// Unscoped return all record including deleted record, refer Soft Delete https://jinzhu.github.io/gorm/crud.html#soft-delete
func (s *DB) Unscoped() *DB {
	return s.clone().search.unscoped().db
}

// Attrs initialize struct with argument if record not found with `FirstOrInit` https://jinzhu.github.io/gorm/crud.html#firstorinit or `FirstOrCreate` https://jinzhu.github.io/gorm/crud.html#firstorcreate
func (s *DB) Attrs(attrs ...interface{}) *DB {
	return s.clone().search.Attrs(attrs...).db
}

// Assign assign result with argument regardless it is found or not with `FirstOrInit` https://jinzhu.github.io/gorm/crud.html#firstorinit or `FirstOrCreate` https://jinzhu.github.io/gorm/crud.html#firstorcreate
func (s *DB) Assign(attrs ...interface{}) *DB {
	return s.clone().search.Assign(attrs...).db
}

// First find first record that match given conditions, order by primary key
func (s *DB) First(out interface{}, where ...interface{}) *DB {
	newScope := s.NewScope(out)
	newScope.Search.Limit(1)

	return newScope.Set("gorm:order_by_primary_key", "ASC").
		inlineCondition(where...).callCallbacks(s.parent.callbacks.queries).db
}

// Take return a record that match given conditions, the order will depend on the database implementation
func (s *DB) Take(out interface{}, where ...interface{}) *DB {
	newScope := s.NewScope(out)
	newScope.Search.Limit(1)
	return newScope.inlineCondition(where...).callCallbacks(s.parent.callbacks.queries).db
}

// Last find last record that match given conditions, order by primary key
func (s *DB) Last(out interface{}, where ...interface{}) *DB {
	newScope := s.NewScope(out)
	newScope.Search.Limit(1)
	return newScope.Set("gorm:order_by_primary_key", "DESC").
		inlineCondition(where...).callCallbacks(s.parent.callbacks.queries).db
}

// Find find records that match given conditions
func (s *DB) Find(out interface{}, where ...interface{}) *DB {
	return s.NewScope(out).inlineCondition(where...).callCallbacks(s.parent.callbacks.queries).db
}

//Preloads preloads relations, don`t touch out
func (s *DB) Preloads(out interface{}) *DB {
	return s.NewScope(out).InstanceSet("gorm:only_preload", 1).callCallbacks(s.parent.callbacks.queries).db
}

// Scan scan value to a struct
func (s *DB) Scan(dest interface{}) *DB {
	return s.NewScope(s.Value).Set("gorm:query_destination", dest).callCallbacks(s.parent.callbacks.queries).db
}

// Row return `*sql.Row` with given conditions
func (s *DB) Row() *sql.Row {
	return s.NewScope(s.Value).row()
}

// Rows return `*sql.Rows` with given conditions
func (s *DB) Rows() (*sql.Rows, error) {
	return s.NewScope(s.Value).rows()
}

// ScanRows scan `*sql.Rows` to give struct
func (s *DB) ScanRows(rows *sql.Rows, result interface{}) error {
	var (
		scope        = s.NewScope(result)
		clone        = scope.db
		columns, err = rows.Columns()
	)

	if clone.AddError(err) == nil {
		scope.scan(rows, columns, scope.Fields())
	}

	return clone.Error
}

// Pluck used to query single column from a model as a map
//     var ages []int64
//     db.Find(&users).Pluck("age", &ages)
func (s *DB) Pluck(column string, value interface{}) *DB {
	return s.NewScope(s.Value).pluck(column, value).db
}

// Count get how many records for a model
func (s *DB) Count(value interface{}) *DB {
	return s.NewScope(s.Value).count(value).db
}

// Related get related associations
func (s *DB) Related(value interface{}, foreignKeys ...string) *DB {
	return s.NewScope(s.Value).related(value, foreignKeys...).db
}

// FirstOrInit find first matched record or initialize a new one with given conditions (only works with struct, map conditions)
// https://jinzhu.github.io/gorm/crud.html#firstorinit
func (s *DB) FirstOrInit(out interface{}, where ...interface{}) *DB {
	c := s.clone()
	if result := c.First(out, where...); result.Error != nil {
		if !result.RecordNotFound() {
			return result
		}
		c.NewScope(out).inlineCondition(where...).initialize()
	} else {
		c.NewScope(out).updatedAttrsWithValues(c.search.assignAttrs)
	}
	return c
}

// FirstOrCreate find first matched record or create a new one with given conditions (only works with struct, map conditions)
// https://jinzhu.github.io/gorm/crud.html#firstorcreate
func (s *DB) FirstOrCreate(out interface{}, where ...interface{}) *DB {
	c := s.clone()
	if result := s.First(out, where...); result.Error != nil {
		if !result.RecordNotFound() {
			return result
		}
		return c.NewScope(out).inlineCondition(where...).initialize().callCallbacks(c.parent.callbacks.creates).db
	} else if len(c.search.assignAttrs) > 0 {
		return c.NewScope(out).InstanceSet("gorm:update_interface", c.search.assignAttrs).callCallbacks(c.parent.callbacks.updates).db
	}
	return c
}

// Update update attributes with callbacks, refer: https://jinzhu.github.io/gorm/crud.html#update
// WARNING when update with struct, GORM will not update fields that with zero value
func (s *DB) Update(attrs ...interface{}) *DB {
	return s.Updates(toSearchableMap(attrs...), true)
}

// Updates update attributes with callbacks, refer: https://jinzhu.github.io/gorm/crud.html#update
func (s *DB) Updates(values interface{}, ignoreProtectedAttrs ...bool) *DB {
	return s.NewScope(s.Value).
		Set("gorm:ignore_protected_attrs", len(ignoreProtectedAttrs) > 0).
		InstanceSet("gorm:update_interface", values).
		callCallbacks(s.parent.callbacks.updates).db
}

// UpdateColumn update attributes without callbacks, refer: https://jinzhu.github.io/gorm/crud.html#update
func (s *DB) UpdateColumn(attrs ...interface{}) *DB {
	return s.UpdateColumns(toSearchableMap(attrs...))
}

// UpdateColumns update attributes without callbacks, refer: https://jinzhu.github.io/gorm/crud.html#update
func (s *DB) UpdateColumns(values interface{}) *DB {
	return s.NewScope(s.Value).
		Set("gorm:update_column", true).
		Set("gorm:save_associations", false).
		InstanceSet("gorm:update_interface", values).
		callCallbacks(s.parent.callbacks.updates).db
}

// Save update value in database, if the value doesn't have primary key, will insert it
func (s *DB) Save(value interface{}) *DB {
	scope := s.NewScope(value)
	if !scope.PrimaryKeyZero() {
		newDB := scope.callCallbacks(s.parent.callbacks.updates).db
		if newDB.Error == nil && newDB.RowsAffected == 0 {
			return s.New().Table(scope.TableName()).FirstOrCreate(value)
		}
		return newDB
	}
	return scope.callCallbacks(s.parent.callbacks.creates).db
}

// Create insert the value into database
func (s *DB) Create(value interface{}) *DB {
	scope := s.NewScope(value)
	return scope.callCallbacks(s.parent.callbacks.creates).db
}

// Delete delete value match given conditions, if the value has primary key, then will including the primary key as condition
// WARNING If model has DeletedAt field, GORM will only set field DeletedAt's value to current time
func (s *DB) Delete(value interface{}, where ...interface{}) *DB {
	return s.NewScope(value).inlineCondition(where...).callCallbacks(s.parent.callbacks.deletes).db
}

// Raw use raw sql as conditions, won't run it unless invoked by other methods
//    db.Raw("SELECT name, age FROM users WHERE name = ?", 3).Scan(&result)
func (s *DB) Raw(sql string, values ...interface{}) *DB {
	return s.clone().search.Raw(true).Where(sql, values...).db
}

// Exec execute raw sql
func (s *DB) Exec(sql string, values ...interface{}) *DB {
	scope := s.NewScope(nil)
	generatedSQL := scope.buildCondition(map[string]interface{}{"query": sql, "args": values}, true)
	generatedSQL = strings.TrimSuffix(strings.TrimPrefix(generatedSQL, "("), ")")
	scope.Raw(generatedSQL)
	return scope.Exec().db
}

// Model specify the model you would like to run db operations
//    // update all users's name to `hello`
//    db.Model(&User{}).Update("name", "hello")
//    // if user's primary key is non-blank, will use it as condition, then will only update the user's name to `hello`
//    db.Model(&user).Update("name", "hello")
func (s *DB) Model(value interface{}) *DB {
	c := s.clone()
	c.Value = value
	return c
}

// Table specify the table you would like to run db operations
func (s *DB) Table(name string) *DB {
	clone := s.clone()
	clone.search.Table(name)
	clone.Value = nil
	return clone
}

// Debug start debug mode
func (s *DB) Debug() *DB {
	return s.clone().LogMode(true)
}

// Transaction start a transaction as a block,
// return error will rollback, otherwise to commit.
func (s *DB) Transaction(fc func(tx *DB) error) (err error) {
	panicked := true
	tx := s.Begin()
	defer func() {
		// Make sure to rollback when panic, Block error or Commit error
		if panicked || err != nil {
			tx.Rollback()
		}
	}()

	err = fc(tx)

	if err == nil {
		err = tx.Commit().Error
	}

	panicked = false
	return
}

// Begin begins a transaction
func (s *DB) Begin() *DB {
	return s.BeginTx(context.Background(), &sql.TxOptions{})
}

// BeginTx begins a transaction with options
func (s *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) *DB {
	c := s.clone()
	if db, ok := c.db.dbSQL.(sqlDb); ok && db != nil {
		tx, err := db.BeginTx(ctx, opts)
		c.db.dbSQL = interface{}(tx).(SQLCommon)

		c.dialect.SetDB(c.db)
		c.AddError(err)
	} else {
		c.AddError(ErrCantStartTransaction)
	}
	return c
}

//NOTE: commit用主库
// Commit commit a transaction
func (s *DB) Commit() *DB {
	var emptySQLTx *sql.Tx
	if db, ok := s.db.dbSQL.(sqlTx); ok && db != nil && db != emptySQLTx {
		s.AddError(db.Commit())
	} else {
		s.AddError(ErrInvalidTransaction)
	}
	return s
}

//NOTE: rollback用主库
// Rollback rollback a transaction
func (s *DB) Rollback() *DB {
	var emptySQLTx *sql.Tx
	if db, ok := s.db.dbSQL.(sqlTx); ok && db != nil && db != emptySQLTx {
		if err := db.Rollback(); err != nil && err != sql.ErrTxDone {
			s.AddError(err)
		}
	} else {
		s.AddError(ErrInvalidTransaction)
	}
	return s
}

// RollbackUnlessCommitted rollback a transaction if it has not yet been
// committed.
func (s *DB) RollbackUnlessCommitted() *DB {
	var emptySQLTx *sql.Tx
	if db, ok := s.db.dbSQL.(sqlTx); ok && db != nil && db != emptySQLTx {
		err := db.Rollback()
		// Ignore the error indicating that the transaction has already
		// been committed.
		if err != sql.ErrTxDone {
			s.AddError(err)
		}
	} else {
		s.AddError(ErrInvalidTransaction)
	}
	return s
}

// 启动一个事务去执行函数f
// 会创建一个有cancel的, 退出f()后cancel, 这个ctx仅仅是为了复用代码, 没有任何其他作用
func (s *DB) DoTx(f func(tx *DB) (err error)) (err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 没ctx就没法trace，所以skip用不到了
	return s.DoTxCtx(ctx, func(ctx context.Context, tx *DB) (err error) {
		return f(tx)
	})
}

// 启动一个事务去执行函数f
// 将你传入的ctx传递下去,
// 这里的ctx
// 这里的ctx只有在捕获了panic或者rollback失败或者commit失败, 才会有用
// 若f()返回了err!=nil或者f()发生panic, 则会rollback
// 否则会commit
func (s *DB) DoTxCtx(ctx context.Context, f func(ctx context.Context, tx *DB) (err error)) (err error) {
	tx := s.Begin()
	defer tx.closeTx(ctx, &err)
	return f(ctx, tx)
}

// 用法:
// func example(ctx context.Context)(err error){
//   tx := db.Begin()
//   defer tx.CloseTx(ctx, &err)
//   if err := tx.Where(xxx). // 执行你的sql语句
//     Table(yyy).
//     UpdateColumns(zzz).
//     Error; err !=nil{
//       return err
//   }
//   return nil
// }
func (s *DB) CloseTx(ctx context.Context, errp *error) {
	s.closeTx(ctx, errp)
}

// skip用于打印调用者所在函数位置
func (s *DB) closeTx(ctx context.Context, errp *error) {
	if xray.GetSegment(ctx) != nil {
		_, seg := xray.BeginSubsegment(ctx, GetSource(3))
		defer func() { seg.Close(*errp) }()
	}

	entry := logrus.WithContext(ctx)
	if r := recover(); r != nil {
		*errp = fmt.Errorf("panic:%v", r) //遇到panic则rollback
		entry.WithError(*errp).Error("panic is captured, then will rollback")
	}

	if *errp != nil {
		if err := s.Rollback().Error; err != nil {
			entry.WithFields(logrus.Fields{
				"error":          (*errp).Error(),
				"rollback_error": err.Error(),
			}).Error("rollback fail")
			*errp = err
		}
	} else {
		if err := s.Commit().Error; err != nil {
			entry.WithField("commit_error", err.Error()).Error("commit fail")
			*errp = err
		}
	}
}

// NewRecord check if value's primary key is blank
func (s *DB) NewRecord(value interface{}) bool {
	return s.NewScope(value).PrimaryKeyZero()
}

// RecordNotFound check if returning ErrRecordNotFound error
func (s *DB) RecordNotFound() bool {
	for _, err := range s.GetErrors() {
		if err == ErrRecordNotFound {
			return true
		}
	}
	return false
}

// CreateTable create table for models
func (s *DB) CreateTable(models ...interface{}) *DB {
	db := s.Unscoped()
	for _, model := range models {
		db = db.NewScope(model).createTable().db
	}
	return db
}

// DropTable drop table for models
func (s *DB) DropTable(values ...interface{}) *DB {
	db := s.clone()
	for _, value := range values {
		if tableName, ok := value.(string); ok {
			db = db.Table(tableName)
		}

		db = db.NewScope(value).dropTable().db
	}
	return db
}

// DropTableIfExists drop table if it is exist
func (s *DB) DropTableIfExists(values ...interface{}) *DB {
	db := s.clone()
	for _, value := range values {
		if s.HasTable(value) {
			db.AddError(s.DropTable(value).Error)
		}
	}
	return db
}

// HasTable check has table or not
func (s *DB) HasTable(value interface{}) bool {
	var (
		scope     = s.NewScope(value)
		tableName string
	)

	if name, ok := value.(string); ok {
		tableName = name
	} else {
		tableName = scope.TableName()
	}

	has := scope.Dialect().HasTable(tableName)
	s.AddError(scope.db.Error)
	return has
}

// AutoMigrate run auto migration for given models, will only add missing fields, won't delete/change current data
func (s *DB) AutoMigrate(values ...interface{}) *DB {
	db := s.Unscoped()
	for _, value := range values {
		db = db.NewScope(value).autoMigrate().db
	}
	return db
}

// ModifyColumn modify column to type
func (s *DB) ModifyColumn(column string, typ string) *DB {
	scope := s.NewScope(s.Value)
	scope.modifyColumn(column, typ)
	return scope.db
}

// DropColumn drop a column
func (s *DB) DropColumn(column string) *DB {
	scope := s.NewScope(s.Value)
	scope.dropColumn(column)
	return scope.db
}

// AddIndex add index for columns with given name
func (s *DB) AddIndex(indexName string, columns ...string) *DB {
	scope := s.Unscoped().NewScope(s.Value)
	scope.addIndex(false, indexName, columns...)
	return scope.db
}

// AddUniqueIndex add unique index for columns with given name
func (s *DB) AddUniqueIndex(indexName string, columns ...string) *DB {
	scope := s.Unscoped().NewScope(s.Value)
	scope.addIndex(true, indexName, columns...)
	return scope.db
}

// RemoveIndex remove index with name
func (s *DB) RemoveIndex(indexName string) *DB {
	scope := s.NewScope(s.Value)
	scope.removeIndex(indexName)
	return scope.db
}

// AddForeignKey Add foreign key to the given scope, e.g:
//     db.Model(&User{}).AddForeignKey("city_id", "cities(id)", "RESTRICT", "RESTRICT")
func (s *DB) AddForeignKey(field string, dest string, onDelete string, onUpdate string) *DB {
	scope := s.NewScope(s.Value)
	scope.addForeignKey(field, dest, onDelete, onUpdate)
	return scope.db
}

// RemoveForeignKey Remove foreign key from the given scope, e.g:
//     db.Model(&User{}).RemoveForeignKey("city_id", "cities(id)")
func (s *DB) RemoveForeignKey(field string, dest string) *DB {
	scope := s.clone().NewScope(s.Value)
	scope.removeForeignKey(field, dest)
	return scope.db
}

// Association start `Association Mode` to handler relations things easir in that mode, refer: https://jinzhu.github.io/gorm/associations.html#association-mode
func (s *DB) Association(column string) *Association {
	var err error
	var scope = s.Set("gorm:association:source", s.Value).NewScope(s.Value)

	if primaryField := scope.PrimaryField(); primaryField.IsBlank {
		err = errors.New("primary key can't be nil")
	} else {
		if field, ok := scope.FieldByName(column); ok {
			if field.Relationship == nil || len(field.Relationship.ForeignFieldNames) == 0 {
				err = fmt.Errorf("invalid association %v for %v", column, scope.IndirectValue().Type())
			} else {
				return &Association{scope: scope, column: column, field: field}
			}
		} else {
			err = fmt.Errorf("%v doesn't have column %v", scope.IndirectValue().Type(), column)
		}
	}

	return &Association{Error: err}
}

// Preload preload associations with given conditions
//    db.Preload("Orders", "state NOT IN (?)", "cancelled").Find(&users)
func (s *DB) Preload(column string, conditions ...interface{}) *DB {
	return s.clone().search.Preload(column, conditions...).db
}

// Set set setting by name, which could be used in callbacks, will clone a new db, and update its setting
func (s *DB) Set(name string, value interface{}) *DB {
	return s.clone().InstantSet(name, value)
}

// InstantSet instant set setting, will affect current db
func (s *DB) InstantSet(name string, value interface{}) *DB {
	s.values.Store(name, value)
	return s
}

// Get get setting by name
func (s *DB) Get(name string) (value interface{}, ok bool) {
	value, ok = s.values.Load(name)
	return
}

// SetJoinTableHandler set a model's join table handler for a relation
func (s *DB) SetJoinTableHandler(source interface{}, column string, handler JoinTableHandlerInterface) {
	scope := s.NewScope(source)
	for _, field := range scope.GetModelStruct().StructFields {
		if field.Name == column || field.DBName == column {
			if many2many, _ := field.TagSettingsGet("MANY2MANY"); many2many != "" {
				source := (&Scope{Value: source}).GetModelStruct().ModelType
				destination := (&Scope{Value: reflect.New(field.Struct.Type).Interface()}).GetModelStruct().ModelType
				handler.Setup(field.Relationship, many2many, source, destination)
				field.Relationship.JoinTableHandler = handler
				if table := handler.Table(s); scope.Dialect().HasTable(table) {
					s.Table(table).AutoMigrate(handler)
				}
			}
		}
	}
}

// AddError add error to the db
func (s *DB) AddError(err error) error {
	if err != nil {
		if err != ErrRecordNotFound {
			if s.logMode == defaultLogMode {
				go s.print("error", fileWithLineNum(), err)
			} else {
				s.log(err)
			}

			errors := Errors(s.GetErrors())
			errors = errors.Add(err)
			if len(errors) > 1 {
				err = errors
			}
		}

		s.Error = err
	}
	return err
}

// GetErrors get happened errors from the db
func (s *DB) GetErrors() []error {
	if errs, ok := s.Error.(Errors); ok {
		return errs
	} else if s.Error != nil {
		return []error{s.Error}
	}
	return []error{}
}

////////////////////////////////////////////////////////////////////////////////
// Private Methods For DB
////////////////////////////////////////////////////////////////////////////////

func (s *DB) clone() *DB {
	db := &DB{
		db:                s.db,
		parent:            s.parent,
		logger:            s.logger,
		logMode:           s.logMode,
		Value:             s.Value,
		Error:             s.Error,
		blockGlobalUpdate: s.blockGlobalUpdate,
		dialect:           newDialect(s.dialect.GetName(), s.db),
		nowFuncOverride:   s.nowFuncOverride,
	}

	s.values.Range(func(k, v interface{}) bool {
		db.values.Store(k, v)
		return true
	})

	if s.search == nil {
		db.search = &search{limit: -1, offset: -1}
	} else {
		db.search = s.search.clone()
	}

	db.search.db = db
	return db
}

func (s *DB) print(v ...interface{}) {
	s.logger.Print(v...)
}

func (s *DB) log(v ...interface{}) {
	if s != nil && s.logMode == detailedLogMode {
		s.print(append([]interface{}{"log", fileWithLineNum()}, v...)...)
	}
}

func (s *DB) slog(sql string, t time.Time, vars ...interface{}) {
	if s.logMode == detailedLogMode {
		s.print("sql", fileWithLineNum(), NowFunc().Sub(t), sql, vars, s.RowsAffected)
	}
}
