# 校园通 School App

给学校用的手机网页 App，三个功能：

1. **到校打卡**：每个学生一张二维码卡。老师或门卫用手机扫一下就完成打卡，超过设定时间会自动标记“迟到”。忘记带卡可以搜索姓名手动打卡。
2. **学校通知**：老师发布通知，可以发给全校，也可以只发给某个班。家长登录就能看到。
3. **学费单据**：家长拍照或上传 PDF 付款单据，学校在后台点“确认收到”或“退回”（附原因），家长能看到处理结果。

家长登录后还能看到孩子**今天几点到校**，以及最近的到校记录。

界面支持 **中文 / English** 切换，手机、电脑都能用，也可以“添加到主屏幕”像 App 一样打开。

## 账号身份

| 身份 | 能做什么 |
|---|---|
| 管理员 | 所有功能，另外可以管理班级、学生、账号，打印学生卡，审核学费单据 |
| 老师（门卫也用这个） | 扫码打卡、查看考勤、导出 CSV、发布通知 |
| 家长 | 看孩子打卡记录、看通知、上传学费单据 |

学生不需要账号，只需要一张二维码卡。

## 快速试用

需要安装 [Go](https://go.dev/dl/) 1.26 或以上。

```bash
cd school-app
ADMIN_PASSWORD=换成你的密码 go run . -demo
```

用浏览器打开 http://localhost:8080 。

`-demo` 会加入示范数据，可以用下面的账号试：

- 管理员：`admin`，密码是上面 `ADMIN_PASSWORD` 设置的
- 老师：`teacher` / `teacher123`
- 家长：`parent` / `parent123`

正式使用时**不要加 `-demo`**。如果不设置 `ADMIN_PASSWORD`，第一次启动会随机生成管理员密码，显示在启动日志里。

## 正式开始使用的步骤

1. 管理员登录 → **管理 → 账号**：新增老师和家长账号
2. **管理 → 班级**：新增班级，选择班主任
3. **管理 → 学生**：新增学生，并“绑定家长”（一个学生可以绑定多个家长，一个家长也可以有多个孩子）
4. **管理 → 学生卡**：选择班级 → 打印，剪下来发给学生（可以过塑或放进卡套）
5. 学生卡丢了：在学生列表点“补办卡”，旧卡会立即作废，再打印新卡

## 到校通知（App 推送 + WhatsApp）

学生扫码打卡后，系统会马上通知这个学生绑定的所有家长：

- **App 推送通知**：家长在“我的”页面点“开启通知”。Android 手机直接可用。**iPhone 要先用 Safari“添加到主屏幕”，再从主屏幕打开 App 才能开启**（iOS 16.4 或以上）。
- **WhatsApp**：发到家长账号里填的电话号码。家长可以在“我的”页面关闭。

同一个学生一天只通知一次，重复扫码不会重复发送。管理员可以在 **管理 → 通知记录** 看到每条通知是否发送成功。

### 开通 WhatsApp（需要 Meta 商业账号）

WhatsApp 不允许普通号码自动发消息，必须使用官方的 WhatsApp Business Platform（Cloud API）：

1. 到 [Meta for Developers](https://developers.facebook.com/) 建立一个 App，加入 “WhatsApp” 产品，并绑定学校的商业账号和一个专用电话号码
2. 在 WhatsApp Manager 新建一个**消息模板**（类别选 “Utility”），名称 `student_checkin`，语言“中文（简体）”，内容例如：

   ```
   您好，{{1}} 已于 {{2}} 到校（{{3}}）。
   ```

   三个变量依次是：学生姓名、到校时间、准时/迟到。等 Meta 审核通过（通常几分钟到一天）。
3. 建立一个**永久访问令牌**（System User access token），并记下电话号码的 **Phone number ID**
4. 启动时设置：

   ```bash
   WHATSAPP_TOKEN=你的令牌 WHATSAPP_PHONE_ID=你的号码ID ./schoolapp -wa-template student_checkin -wa-lang zh_CN -country-code 60 ...
   ```

5. 用管理员登录 → **管理 → 通知记录** → 输入自己的号码点“发送测试”

费用：Meta 按每条模板消息收费（Utility 类别，价格看国家）。家长电话可以填本地格式（如 `012-3456789`），系统会自动加上 `-country-code` 国家码。

## 部署到服务器

```bash
cd school-app
go build -o schoolapp .
./schoolapp -data /var/lib/schoolapp -addr :8080 -behind-https
```

⚠️ **一定要用 HTTPS**。手机浏览器只允许 `https://` 网站使用相机，否则无法扫码。最简单的办法是在前面放 [Caddy](https://caddyserver.com/)，它会自动申请免费证书：

```
# Caddyfile
school.你的域名.com {
    reverse_proxy localhost:8080
}
```

也可以直接用 `-tls-cert` 和 `-tls-key` 指定证书文件。

### 启动参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-addr` | `:8080` | 监听地址 |
| `-data` | `./data` | 数据库和上传文件存放的文件夹 |
| `-tz` | `Asia/Kuala_Lumpur` | 学校所在时区（UTC+8），例如 `Asia/Shanghai`、`Asia/Singapore` |
| `-late` | `07:30` | 超过这个时间打卡算迟到 |
| `-currency` | `RM` | 金额前显示的货币 |
| `-behind-https` | 关 | 用 Caddy/Nginx 提供 HTTPS 时打开 |
| `-tls-cert` / `-tls-key` | 无 | 直接使用 HTTPS 证书 |
| `-push-contact` | `admin@example.com` | 学校联络邮箱（推送服务需要），请改成真实邮箱 |
| `-wa-template` | `student_checkin` | WhatsApp 模板名称 |
| `-wa-lang` | `zh_CN` | WhatsApp 模板语言 |
| `-country-code` | `60` | 本地电话号码要加的国家码 |
| `-demo` | 关 | 加入示范数据 |

### 备份

所有资料都在 `-data` 文件夹里：`school.db`（数据库）和 `uploads/`（家长上传的单据）。**请定期备份整个文件夹。**

## 安全说明

- 密码使用 bcrypt 加密保存；连续输错 8 次会锁定 15 分钟
- 二维码里是随机密码，不是学号，无法自己伪造别人的卡
- 家长只能看到自己孩子的资料和单据；单据文件需要登录才能打开
- 上传文件只接受 JPG / PNG / WEBP / PDF，最大 10 MB

## 技术结构

- 后端：Go + SQLite，编译出来是一个程序文件，网页也打包在里面
- 前端：原生 JavaScript，没有构建步骤（`web/` 文件夹）
- 扫码：[jsQR](https://github.com/cozmo/jsQR)（Apache-2.0），在手机浏览器里识别二维码

## 以后可以加的功能

- 发布通知时也推送给家长
- 放学签退
- 用 Excel / CSV 批量导入学生
- 请假申请
- 打包成 iOS / Android App 上架
