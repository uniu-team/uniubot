# 消息发送

SkiBot 的消息系统分为**公共消息段**和**平台独有消息段**两层。

- 公共方法（`text` `at` `reply` `image`）表达语义，所有平台通用
- 平台独有方法通过 `MessageSegment.raw()` 发送，插件端按 `event.adapter_id` 判断目标平台

各适配器在发送阶段负责将核心消息段转换为对应平台格式。

---

## 基础用法

### 在命令回调中回复消息

回调参数：

| 参数      | 类型         | 说明                         |
| --------- | ------------ | ---------------------------- |
| `args`    | `ParsedArgs` | 命令行参数                   |
| `handler` | `Handler`    | 调用 `handler.finish(msg)` 回复 |
| `msg`     | `Message`    | 空消息对象，往里面加消息段     |
| `event`   | `BotMessageEvent` | 当前事件，包含 `adapter_id` 等 |

```ts
import type { ParsedArgs } from '../../dist/app/argParser.js';
import type { Handler } from '../../dist/app/bot.js';
import type { Message } from '../../dist/app/messages.js';
import { MessageSegment } from '../../dist/app/messages.js';

ctx.bot.command('hello', '打个招呼', async (
    args: ParsedArgs,
    handler: Handler,
    msg: Message,
    event
) => {
    msg.addMessage(MessageSegment.reply(event.message_id));
    msg.addMessage(MessageSegment.text('Hello, world!'));
    handler.finish(msg);
});
```

### 在事件回调中回复

```ts
ctx.bot.on('message', async (event, handler, msg) => {
    msg.addMessage(MessageSegment.text('收到消息'));
    handler.finish(msg);
});
```

---

## 公共消息段 API

所有公共方法都定义在 `app/messages.ts` 的 `MessageSegment` 上，返回 `Message` 实例。

### MessageSegment.text(text)

```ts
MessageSegment.text('这是一段文本')
// { type: "text", data: { text: "这是一段文本" } }
```

### MessageSegment.at(user_id)

```ts
MessageSegment.at(123456)
// { type: "at", data: { user_id: "123456" } }
```

### MessageSegment.reply(id)

```ts
MessageSegment.reply('msg_abcdef')
// { type: "reply", data: { id: "msg_abcdef" } }
```

### MessageSegment.image(file)

```ts
MessageSegment.image('https://example.com/pic.png')
// { type: "image", data: { file: "https://example.com/pic.png" } }
```

### Message.addMessage()

往消息里追加一个或多个消息段：

```ts
msg.addMessage(MessageSegment.text('第一行'));
msg.addMessage(MessageSegment.text('\n第二行'));
```

`addMessage` 接受 `Message` 或 `BaseMessage` 实例，会展开合并。

---

## 平台独有消息段

对于只有某个平台支持的消息类型，使用 `MessageSegment.raw(platform, type, data)`，并在插件里通过 `event.adapter_id` 判断当前适配器。

```ts
MessageSegment.raw(platform, type, data)
// { type: "raw", data: { platform, type, data } }
```

适配器在发送阶段识别 `type === 'raw'` 且 `platform` 匹配自己，展开为对应平台的真实消息段；其他适配器忽略。

### 检测当前适配器

`event.adapter_id` 记录了事件来源适配器的 id：

| 适配器      | `adapter_id`   |
| ----------- | -------------- |
| OneBot HTTP | `onebotHttp`   |
| WebChat     | `webchat`      |

插件端写法：

```ts
ctx.bot.command('demo', '演示平台独有消息', async (args, handler, msg, event) => {
    msg.addMessage(MessageSegment.text('公共文本'));

    if (event.adapter_id === 'onebotHttp') {
        msg.addMessage(MessageSegment.raw('onebot', 'image', {
            file: 'https://example.com/b.png',
        }));
    }

    handler.finish(msg);
});
```

---

## OneBot 平台独有消息段

```ts
// 仅在 OneBot 平台生效，通过 event.adapter_id === 'onebotHttp' 判断
msg.addMessage(MessageSegment.raw('onebot', '<OneBot 消息类型>', { ... }));
```

### 图片

```ts
msg.addMessage(MessageSegment.raw('onebot', 'image', {
    file: 'https://example.com/image.png',       // 必填
    type: 'flash',                                // 可选，闪照
    url: 'https://example.com/image.png',         // 可选
    cache: true,                                  // 可选
    proxy: true,                                  // 可选
    timeout: 10,                                  // 可选
}));
```

### 语音

```ts
msg.addMessage(MessageSegment.raw('onebot', 'record', {
    file: 'https://example.com/voice.mp3',
    magic: false,   // 变声
    url: '',
    cache: true,
    proxy: true,
    timeout: 10,
}));
```

### 短视频

```ts
msg.addMessage(MessageSegment.raw('onebot', 'video', {
    file: 'https://example.com/video.mp4',
    cover: 'https://example.com/cover.jpg',  // 封面
    c: 2,                                     // 网络视频
}));
```

### 音乐 / 大表情 / 窗口抖动

```ts
msg.addMessage(MessageSegment.raw('onebot', 'music', {
    type: 'qq',
    id: 'musicid',
}));

msg.addMessage(MessageSegment.raw('onebot', 'face', {
    id: 12,
}));

msg.addMessage(MessageSegment.raw('onebot', 'shake', {}));
```

### 合并转发

```ts
const nodes = [
    {
        type: 'node',
        data: {
            user_id: '123456',
            nickname: '用户A',
            content: MessageSegment.text('第一段').json(),
        },
    },
    {
        type: 'node',
        data: {
            user_id: '7890',
            nickname: '用户B',
            content: MessageSegment.text('第二段').json(),
        },
    },
];

msg.addMessage(MessageSegment.raw('onebot', 'forward', {
    id: 'forward_id',    // 可选，直接引用已有转发
    messages: nodes,
}));
```

### 其他 OneBot 消息段

```ts
// JSON 消息
msg.addMessage(MessageSegment.raw('onebot', 'json', {
    data: '{"app":"com.example"}',
}));

// XML 消息
msg.addMessage(MessageSegment.raw('onebot', 'xml', {
    data: '<msg>...</msg>',
}));

// 掷骰子
msg.addMessage(MessageSegment.raw('onebot', 'dice', {}));

// 猜拳
msg.addMessage(MessageSegment.raw('onebot', 'rps', {}));

// 位置
msg.addMessage(MessageSegment.raw('onebot', 'location', {
    lat: 39.9042,
    lon: 116.4074,
    title: '北京',
    content: '天安门',
}));

// 推荐好友 / 推荐群
msg.addMessage(MessageSegment.raw('onebot', 'contact', {
    type: 'qq',   // qq | group
    id: '12345',
}));

// 分享链接
msg.addMessage(MessageSegment.raw('onebot', 'share', {
    url: 'https://example.com',
    title: '标题',
    content: '内容',
    image: 'https://example.com/pic.png',
}));

// JSON 卡片消息（需要 OneBot 服务端支持）
msg.addMessage(MessageSegment.raw('onebot', 'json', {
    data: JSON.stringify({
        app: 'com.tencent.structmsg',
        desc: '描述',
        view: 'view_8C8E89B49BE609866298ADDFF2DBABA4',
        prompt: '提示',
        meta: {
            news: {
                title: '标题',
                jumpUrl: 'https://example.com',
                preview: 'https://example.com/pic.png',
                tag: '标签',
            },
        },
        config: {
            ctime: Math.floor(Date.now() / 1000),
            forward: true,
            autosize: true,
        },
    }),
}));
```

---

## 完整示例

```ts
import { BasePlugin, ConfigField } from '../../dist/app/pluginBase.js';
import type { ParsedArgs } from '../../dist/app/argParser.js';
import type { Handler } from '../../dist/app/bot.js';
import type { Message } from '../../dist/app/messages.js';
import type { PluginContext, IHandler } from '../../dist/app/types.js';
import { MessageSegment } from '../../dist/app/messages.js';

export default class Demo extends BasePlugin {
    @ConfigField({ type: 'boolean', default: true, description: '是否启用' })
    enabled = true;

    async onLoad(ctx: PluginContext): Promise<void> {
        // 公共消息：所有平台通用
        ctx.bot.command('info', '基本信息', async (
            args: ParsedArgs,
            handler: Handler,
            msg: Message,
            event
        ) => {
            msg.addMessage(MessageSegment.reply(event.message_id));
            msg.addMessage(MessageSegment.at(event.user_id));
            msg.addMessage(MessageSegment.text(`\nSkiBot v${ctx.version}`));
            handler.finish(msg);
        });

        // 获取图片
        ctx.bot.command('pic', '发送图片', async (
            args: ParsedArgs,
            handler: Handler,
            msg: Message,
            event
        ) => {
            const url = args._[0];

            if (!url) {
                msg.addMessage(MessageSegment.text('请提供图片地址，如 /pic https://example.com/a.png'));
                handler.finish(msg);
                return;
            }

            msg.addMessage(MessageSegment.image(url));
            handler.finish(msg);
        });

        // 平台独有：OneBot 戳一戳
        ctx.bot.command('poke', '戳一戳某人', async (
            args: ParsedArgs,
            handler: Handler,
            msg: Message,
            event
        ) => {
            if (event.adapter_id !== 'onebotHttp') {
                msg.addMessage(MessageSegment.text('此命令仅支持 OneBot 平台'));
                handler.finish(msg);
                return;
            }

            const target = args._[0] || String(event.user_id);

            msg.addMessage(MessageSegment.raw('onebot', 'poke', {
                qq: target,
            }));

            handler.finish(msg);
        });
    }

    async onUnload(): Promise<void> {}
}
```

---

## 适配器发送转换

各适配器在 `send_group_msg` / `send_private_msg` 中负责将核心 `Message` 转为平台格式。

### OneBot

`adapters/onebotHttp/messages.ts` 的 `toOneBotMessage()`:

- `text` → 原样透传
- `at` → `{ user_id }` 转为 `{ qq }`
- `reply` → 原样透传
- `image` → 原样透传
- `raw` 且 `platform === 'onebot'` → 展开为 `{ type, data }`
- 其他 → 原样透传

### WebChat

`adapters/webchat/index.ts` 只提取各段 `data.text` 拼接输出，非文本段自动忽略。

---

## 不适合的场景

以下场景目前不支持，需要重新设计：

- 主动发送时没有事件上下文，无法判断要用哪个适配器
  - 当前只能通过事件回调被动回复
