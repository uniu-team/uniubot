# 适配器自定义事件

SkiBot 允许适配器导出平台专属的事件和发送者类型，同时主程序保持统一的事件模型。插件在访问平台专属字段前，应使用适配器提供的类型守卫进行判断。

## 核心模型

所有事件必须继承 `app/events.ts` 中的 `BotEvent`。消息事件可以继承或复用泛型消息事件类：

```ts
import { BotEvent, GroupMessageEvent, MessageSender } from '../../app/events.js';

type MyAdapterId = 'myAdapter';

interface MySenderExtra {
    display_name?: string;
    role?: string;
}

export type MySender = MessageSender<MySenderExtra>;
export type MyGroupMessageEvent = GroupMessageEvent<MyAdapterId, MySender>;
```

每个事件都包含以下核心字段：

```ts
event.time;        // 时间戳
event.self_id;     // 机器人自身 ID
event.adapter_id;  // 适配器标识，用于平台判断
event.adapter;     // 适配器实例（可发送消息）
event.raw_event;   // 原始上报数据
```

`adapter_id` 是平台身份的唯一标识。除非适配器有独立的协议层理由，否则不要添加额外的 platform 字段。

## 适配器类型导出

适配器应在 `adapters/<adapter>/events.ts` 中导出平台专属的类型和守卫函数。

示例：

```ts
import type { BotEvent, MessageSender } from '../../app/events.js';
import type { PrivateMessageEvent } from '../../app/events.js';

export type MyAdapterId = 'myAdapter';

export interface MySenderExtra {
    display_name?: string;
    role?: 'admin' | 'member';
}

export type MySender = MessageSender<MySenderExtra>;
export type MyPrivateMessageEvent = PrivateMessageEvent<MyAdapterId, MySender>;

export function isMyAdapterEvent(event: BotEvent): event is BotEvent<MyAdapterId> {
    return event.adapter_id === 'myAdapter';
}

export function isMyPrivateMessageEvent(event: BotEvent): event is MyPrivateMessageEvent {
    return isMyAdapterEvent(event)
        && (event as any).message_type === 'private'
        && typeof (event as any).user_id === 'number'
        && typeof (event as any).sender?.user_id === 'number';
}
```

**守卫函数推荐使用结构化判断，而非 `instanceof`。** 适配器热重载会创建不同的类身份，导致 `instanceof` 失效；结构化判断则始终稳定。

## 发送自定义事件

适配器通过 `AdapterContext.emitEvent` 发送事件：

```ts
this.ctx.emitEvent('myAdapter.connected', event);
```

适配器专属的事件应使用命名空间格式：

```txt
myAdapter.connected
myAdapter.disconnected
myAdapter.callback_query
```

内置事件名用于通用事件分类，不要占用：

```txt
message
notice
request
meta_event
```

## 插件使用方式

插件可以从 `dist/adapters/<adapter>/events.js` 导入适配器类型。

推荐写法（守卫优先）：

```ts
import type { PluginContext } from '../../dist/app/types.js';
import { isOneBotGroupMessageEvent } from '../../dist/adapters/onebotHttp/events.js';

export default class DemoPlugin {
    async onLoad(ctx: PluginContext): Promise<void> {
        ctx.bot.on('message', async (event) => {
            if (!isOneBotGroupMessageEvent(event)) return;

            const card = event.sender.card;
            const role = event.sender.role;
        });
    }
}
```

守卫同时检查适配器标识和事件结构。通过守卫后，TypeScript 会将事件类型收窄为适配器专属类型，可以直接访问平台字段。

## 泛型监听

`bot.on` 也支持显式指定泛型事件类型：

```ts
import type { OneBotGroupMessageEvent } from '../../dist/adapters/onebotHttp/events.js';

ctx.bot.on<OneBotGroupMessageEvent>('message', async (event) => {
    if (event.adapter_id !== 'onebotHttp') return;
    event.sender.card;
});
```

这种方式写法简洁，但不会在运行时验证事件的实际类型。访问平台专属字段时，守卫写法更安全。

## 内置四种事件类型的扩展

主程序提供四种内置事件类型，均支持适配器扩展：

| 事件类型 | 扩展机制 | 说明 |
|----------|----------|------|
| `BotMessageEvent` / `GroupMessageEvent` / `PrivateMessageEvent` | `TSender` 泛型 | 扩展 sender 字段 |
| `NoticeEvent` | 子类化 | 添加通知专属字段 |
| `RequestEvent` | 子类化 | 添加请求专属字段 |
| `MetaEvent` | 子类化 | 添加元事件专属字段 |

### 消息事件：通过 TSender 扩展

消息事件使用 `TSender` 泛型参数扩展发送者字段：

```ts
import type { MessageSender } from '../../app/events.js';

interface MySenderExtra {
    display_name?: string;
    role?: 'admin' | 'member';
}

export type MySender = MessageSender<MySenderExtra>;
export type MyGroupMessageEvent = GroupMessageEvent<'myAdapter', MySender>;
```

插件使用时，通过守卫收窄后即可访问扩展字段：

```ts
if (isMyGroupMessageEvent(event)) {
    event.sender.display_name;  // OK
    event.sender.role;          // OK
}
```

适配器应将协议专属的 sender 字段放在自己导出的 sender 类型中。不要把仅某个平台使用的字段添加到主程序 sender，除非该字段在多平台间普遍存在。

### Notice / Request / Meta 事件：通过子类化扩展

这三种事件类型的协议专属字段差异较大（如 OneBot 的群禁言通知有 `duration`，好友请求有 `comment`/`flag`），不适合用泛型参数表达，应通过继承子类化来扩展：

```ts
import { NoticeEvent, RequestEvent, MetaEvent } from '../../app/events.js';
import type { ISendMessage } from '../../app/types.js';

// 扩展 NoticeEvent
export class OneBotGroupBanNoticeEvent extends NoticeEvent<'onebotHttp'> {
    group_id: number;
    user_id: number;
    operator_id: number;
    duration: number;

    constructor(
        time: number, self_id: number, adapter: ISendMessage | null,
        notice_type: string, group_id: number, user_id: number,
        operator_id: number, duration: number
    ) {
        super(time, self_id, 'onebotHttp', adapter, notice_type);
        this.group_id = group_id;
        this.user_id = user_id;
        this.operator_id = operator_id;
        this.duration = duration;
    }
}

// 扩展 RequestEvent
export class OneBotFriendRequestEvent extends RequestEvent<'onebotHttp'> {
    user_id: number;
    comment: string;
    flag: string;

    constructor(
        time: number, self_id: number, adapter: ISendMessage | null,
        user_id: number, comment: string, flag: string
    ) {
        super(time, self_id, 'onebotHttp', adapter, 'friend');
        this.user_id = user_id;
        this.comment = comment;
        this.flag = flag;
    }
}

// 扩展 MetaEvent
export class OneBotHeartbeatEvent extends MetaEvent<'onebotHttp'> {
    status: Record<string, unknown>;

    constructor(
        time: number, self_id: number, adapter: ISendMessage | null,
        status: Record<string, unknown>
    ) {
        super(time, self_id, 'onebotHttp', adapter, 'heartbeat');
        this.status = status;
    }
}
```

配套的类型守卫同样使用结构化判断：

```ts
export function isOneBotGroupBanNoticeEvent(event: BotEvent): event is OneBotGroupBanNoticeEvent {
    return event.adapter_id === 'onebotHttp'
        && (event as any).notice_type === 'group_ban'
        && typeof (event as any).group_id === 'number'
        && typeof (event as any).duration === 'number';
}

export function isOneBotFriendRequestEvent(event: BotEvent): event is OneBotFriendRequestEvent {
    return event.adapter_id === 'onebotHttp'
        && (event as any).request_type === 'friend'
        && typeof (event as any).user_id === 'number'
        && typeof (event as any).flag === 'string';
}
```

插件使用：

```ts
ctx.bot.on('notice', async (event) => {
    if (!isOneBotGroupBanNoticeEvent(event)) return;
    console.log(event.duration);   // OK
    console.log(event.operator_id); // OK
});

ctx.bot.on('request', async (event) => {
    if (!isOneBotFriendRequestEvent(event)) return;
    console.log(event.comment); // OK
    console.log(event.flag);    // OK
});
```

## OneBot 未知事件处理

OneBot HTTP 适配器会将未知的 `post_type` 上报包装为 `OneBotRawEvent`，并以如下格式发送事件：

```txt
onebotHttp.<post_type>
```

如果 `post_type` 缺失，事件名为：

```txt
onebotHttp.unknown
```

插件可以监听这些原始事件：

```ts
import { isOneBotRawEvent } from '../../dist/adapters/onebotHttp/events.js';

ctx.bot.on('onebotHttp.some_custom_type', async (event) => {
    if (!isOneBotRawEvent(event)) return;
    const raw = event.raw_event;
});
```

## 现有适配器导出一览

### OneBot HTTP (`adapters/onebotHttp/events.ts`)

| 导出 | 说明 |
|------|------|
| `OneBotAdapterId` | 适配器标识字面量类型 `'onebotHttp'` |
| `OneBotSenderExtra` | OneBot 专属 sender 扩展字段 |
| `OneBotSender` | `MessageSender<OneBotSenderExtra>` |
| `OneBotMessageEvent` | OneBot 消息事件 |
| `OneBotGroupMessageEvent` | OneBot 群消息事件 |
| `OneBotPrivateMessageEvent` | OneBot 私聊消息事件 |
| `OneBotNoticeEvent` | OneBot 通知事件 |
| `OneBotRequestEvent` | OneBot 请求事件 |
| `OneBotMetaEvent` | OneBot 元事件 |
| `OneBotRawEvent` | OneBot 未知上报事件类 |
| `isOneBotEvent` | 判断是否来自 OneBot 适配器 |
| `isOneBotMessageEvent` | 判断是否为 OneBot 消息事件 |
| `isOneBotGroupMessageEvent` | 判断是否为 OneBot 群消息事件 |
| `isOneBotPrivateMessageEvent` | 判断是否为 OneBot 私聊消息事件 |
| `isOneBotRawEvent` | 判断是否为 OneBot 原始未知事件 |

### WebChat (`adapters/webchat/events.ts`)

| 导出 | 说明 |
|------|------|
| `WebChatAdapterId` | 适配器标识字面量类型 `'webchat'` |
| `WebChatSenderExtra` | WebChat 专属 sender 扩展字段 |
| `WebChatSender` | `MessageSender<WebChatSenderExtra>` |
| `WebChatPrivateMessageEvent` | WebChat 私聊消息事件 |
| `isWebChatEvent` | 判断是否来自 WebChat 适配器 |
| `isWebChatPrivateMessageEvent` | 判断是否为 WebChat 私聊消息事件 |
