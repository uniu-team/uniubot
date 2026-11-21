# 数据库系统使用说明

SkiBot 使用基于 SQL 的 ORM 风格数据库系统，不再使用旧的 JSON 文件 Key-Value 存储。数据库层默认支持 SQLite，并提供 PostgreSQL 驱动接口。所有数据以结构化列形式存储，通过 `Repository<T>` 进行类型安全的数据访问。

## 配置方式

在 `config/config.yml` 中配置数据库：

```yaml
database:
  engine: sqlite # sqlite | pgsql
  sqlite:
    file: ./data/skibot.sqlite
  pgsql:
    connectionString: postgres://user:password@localhost:5432/skibot
    pool:
      min: 2
      max: 10
```

对于单机 Bot 部署，推荐默认使用 SQLite。它不需要额外启动数据库服务，部署成本最低。

## 核心概念

### 作用域隔离

数据库 API 通过 `ScopedDatabaseClient` 暴露。这个客户端带有"作用域"，插件和适配器拿到的都不是全局数据库连接，而是只能访问自己命名空间的数据库客户端。

所有表名都会自动添加作用域前缀：

| 调用方 | 获取方式 | 实际表名前缀 |
|---|---|---|
| 核心服务 | `databaseManager.core('counter')` | `core_counter_` |
| 插件 | `databaseManager.plugin('weather')` | `plugin_weather_` |
| 适配器 | `databaseManager.adapter('onebot-http')` | `adapter_onebot_http_` |

例如，一个名为 `weather` 的插件执行：

```ts
await db.insert('cache', { key: 'latest', value: 'sunny' });
```

实际写入的表是 `plugin_weather_cache`。插件和适配器通过框架提供的数据库 API 时，无法读写其他插件、适配器或核心模块的表。

### ORM 风格 Repository

框架引入了 `Repository<T>` 类，对 `ScopedDatabaseClient` 进行了一层类型安全的 ORM 封装。每个 Repository 对应一张表，提供实体风格的 CRUD 操作，建议在插件、适配器和核心服务中使用 Repository 而不是直接使用 `ScopedDatabaseClient`。

## Repository API

```ts
import { Repository } from './app/database/index.js';
```

### 创建 Repository

```ts
interface ItemRow {
    id?: number;
    name: string;
    enabled: number;
    created_at: number;
}

const itemRepo = new Repository<ItemRow>(db, 'items', {
    schema: {
        columns: {
            id: { type: 'integer', primaryKey: true, autoIncrement: true },
            name: { type: 'text', notNull: true },
            enabled: { type: 'integer', notNull: true, default: 1 },
            created_at: { type: 'integer', notNull: true },
        },
    },
});
```

构造参数：
- `db` — `ScopedDatabaseClient` 实例（插件拿到的第三个参数、适配器的 `ctx.db`）
- `'items'` — 本地表名（框架自动加作用域前缀）
- `options.schema` — 表结构定义，首次使用时自动建表
- `options.autoCreate` — 是否自动建表，默认 `true`

### 写入

```ts
await itemRepo.create({ name: 'example', enabled: 1, created_at: Date.now() });
```

单行插入，返回写入的数据对象（含提供的主键值）。

```ts
await itemRepo.createMany([
    { name: 'a', enabled: 1, created_at: Date.now() },
    { name: 'b', enabled: 1, created_at: Date.now() },
]);
```

批量插入，内部使用事务，全部成功或全部回滚。

### 查询

```ts
const rows = await itemRepo.find(
    { enabled: 1 },
    { orderBy: 'created_at', order: 'desc', limit: 20 }
);
```

按条件查询，返回 `T[]`。`where` 支持等值条件，多字段以 `AND` 拼接。

```ts
const row = await itemRepo.findOne({ name: 'example' });
```

查询单行，返回 `T | null`。

```ts
const all = await itemRepo.findAll({ orderBy: 'id', order: 'asc' });
```

查询全部（无条件），返回 `T[]`。

### 更新

```ts
const affected = await itemRepo.update(
    { enabled: 0 },
    { name: 'example' }
);
```

第一个参数是要更新的列和值，第二个参数是 `WHERE` 条件。返回受影响行数。空的 `where` 条件会抛出错误，防止意外全表更新。

### 删除

```ts
const affected = await itemRepo.delete({ name: 'example' });
```

按条件删除，返回受影响行数。空的 `where` 条件会抛出错误。

```ts
const affected = await itemRepo.deleteAll();
```

删除表中所有行。此方法没有安全保护，调用时需确认。

### 其他

```ts
const n = await itemRepo.count();           // 计数全部
const n = await itemRepo.count({ enabled: 1 }); // 条件计数
const exists = await itemRepo.hasTable();   // 检查表是否存在
await itemRepo.ensureTable();               // 确保表已创建
await itemRepo.dropTable();                 // 删除表
```

## 插件中使用数据库

插件在 `init` 函数的第三个参数拿到 `db: ScopedDatabaseClient | null`。推荐通过 `Repository` 使用：

```ts
import { Repository } from '../app/database/index.js';

interface CacheRow {
    key: string;
    value: string;
    updated_at: number;
}

export async function init(bot, adapterManager, db) {
    if (!db) return;

    const cacheRepo = new Repository<CacheRow>(db, 'cache', {
        schema: {
            columns: {
                key: { type: 'text', primaryKey: true },
                value: { type: 'text', notNull: true },
                updated_at: { type: 'integer', notNull: true },
            },
        },
    });

    // 写入
    await cacheRepo.create({
        key: 'latest',
        value: JSON.stringify({ ok: true }),
        updated_at: Date.now(),
    });

    // 查询
    const row = await cacheRepo.findOne({ key: 'latest' });

    // 更新
    await cacheRepo.update(
        { value: JSON.stringify({ ok: false }), updated_at: Date.now() },
        { key: 'latest' }
    );

    // 全量查询
    const all = await cacheRepo.findAll();
}
```

如果插件名称是 `weather`，上面的 `cache` 表实际创建为 `plugin_weather_cache`。插件不需要、也不应该关心真实表名。

## 适配器中使用数据库

适配器可以从 `AdapterContext.db` 获取数据库客户端：

```ts
import { Repository } from './app/database/index.js';

interface SessionRow {
    id: string;
    payload: string;
    updated_at: number;
}

async start(ctx: AdapterContext): Promise<void> {
    if (!ctx.db) return;

    const sessionRepo = new Repository<SessionRow>(ctx.db, 'sessions', {
        schema: {
            columns: {
                id: { type: 'text', primaryKey: true },
                payload: { type: 'text', notNull: true },
                updated_at: { type: 'integer', notNull: true },
            },
        },
    });

    await sessionRepo.create({
        id: this.id,
        payload: JSON.stringify(this.config),
        updated_at: Date.now(),
    });
}
```

如果适配器 ID 是 `onebot-http`，上面的 `sessions` 表实际创建为 `adapter_onebot_http_sessions`。

注意：`AdapterContext.db` 的类型是 `ScopedDatabaseClient | null`。如果某些代码路径没有提供数据库上下文（例如通过 `bot.useAdapter()` 而不是 `AdapterManager`），适配器需要先判断 `ctx.db` 是否存在。

## 核心服务中使用数据库

核心服务直接通过 `databaseManager` 获取作用域客户端，然后创建 Repository：

```ts
import databaseManager from './app/database/manager.js';
import { Repository } from './app/database/repository.js';

const db = databaseManager.core('counter');
const repo = new Repository<T>(db, 'table_name', { schema });
```

当前计数器服务使用的表：

```text
core_counter_groups
core_counter_users
core_counter_messages
core_counter_daily_stats
```

核心服务之间也建议使用不同 namespace，避免把所有核心数据混在同一个前缀下。

## ScopedDatabaseClient 底层 API

如果你的场景不适合使用 Repository（例如需要写更灵活的查询），可以直接使用 `ScopedDatabaseClient`：

```ts
await db.createTable('table_name', schema);
await db.dropTable('table_name');
await db.insert('table_name', data);
await db.insertMany('table_name', rows);
await db.update('table_name', data, where);
await db.delete('table_name', where);
await db.deleteAll('table_name');
await db.select('table_name', options);
await db.selectOne('table_name', where);
await db.count('table_name', where);
await db.hasTable('table_name');
```

建议优先使用 `Repository`，它提供类型安全和更明确的调用风格。

## 表名规则

插件、适配器和核心服务都只需要传入"本地表名"：

```ts
await db.createTable('settings', schema);
await db.select('settings');
```

不要传入完整物理表名：

```ts
await db.select('plugin_other_settings'); // 不应该这样使用
await db.select('core_counter_messages'); // 不应该这样使用
```

表名必须符合 `^[a-zA-Z_][a-zA-Z0-9_]*$`，即只能包含字母、数字、下划线，不能以数字开头。

## Schema 示例

```ts
await db.createTable('items', {
    columns: {
        id: { type: 'integer', primaryKey: true, autoIncrement: true },
        name: { type: 'text', notNull: true },
        enabled: { type: 'boolean', notNull: true, default: 1 },
        created_at: { type: 'integer', notNull: true },
    },
    indexes: [
        {
            name: 'name',
            columns: ['name'],
        },
    ],
});
```

字段定义支持两种写法。简写：

```ts
columns: {
    name: 'text',
    created_at: 'integer',
}
```

完整写法：

```ts
columns: {
    id: { type: 'integer', primaryKey: true, autoIncrement: true },
    name: { type: 'text', notNull: true },
}
```

## 字段类型映射

推荐使用以下可移植类型，方便未来在 SQLite 和 PostgreSQL 之间切换：

| 类型 | SQLite | PostgreSQL |
|---|---|---|
| `text` | `TEXT` | `TEXT` |
| `integer` | `INTEGER` | `INTEGER` |
| `real` | `REAL` | `REAL` |
| `blob` | `BLOB` | `BYTEA` |
| `json` | `TEXT` | `JSONB` |
| `boolean` | `INTEGER` | `BOOLEAN` |
| `serial` | `INTEGER PRIMARY KEY AUTOINCREMENT` | `SERIAL PRIMARY KEY` |

如果需要数据库特定类型，也可以直接传入 SQL 类型字符串，但这会降低跨数据库兼容性。

## SQLite 说明

SQLite 驱动使用 `better-sqlite3`，初始化时会启用：

```text
journal_mode = WAL
foreign_keys = ON
busy_timeout = 5000
```

默认数据库文件为 `./data/skibot.sqlite`。

## PostgreSQL 说明

PostgreSQL 驱动通过 `pg` 包实现。使用 `database.engine: pgsql` 前需要安装依赖：

```bash
npm install pg
```

然后在配置中填写连接字符串：

```yaml
database:
  engine: pgsql
  pgsql:
    connectionString: postgres://user:password@localhost:5432/skibot
```

## 安全边界

作用域数据库客户端和 Repository 可以保证：

- 插件通过框架数据库 API 时，只能访问自己的 `plugin_<name>_*` 表
- 适配器通过框架数据库 API 时，只能访问自己的 `adapter_<id>_*` 表
- 核心服务通过框架数据库 API 时，只能访问自己的 `core_<namespace>_*` 表
- 插件和适配器无法通过该 API 执行 raw SQL
- `delete()` 和 `update()` 的空 `where` 条件会被拒绝，减少误操作风险
- `Repository<T>` 提供类型安全，在编译期拦截字段拼写错误

但这不是进程级沙箱。只要插件代码可以直接使用 Node.js 能力，它仍然可能自行导入 `fs`、网络模块、数据库驱动或读取环境变量。如果需要防御恶意插件，需要额外做强隔离：

- 独立进程
- Worker / VM 沙箱
- 容器隔离
- 数据库账号级权限控制
- 每个插件单独 schema 或单独数据库账号

当前数据库系统的目标是提供框架级的规范 ORM API 和表级命名空间隔离，而不是替代完整安全沙箱。
