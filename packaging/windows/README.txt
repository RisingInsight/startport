StarPort 一键安装包（Windows x64）

适用：Intel / AMD 64 位 Windows 10、Windows 11。
这是从当前项目源码编译的本地离线安装包，不是官方签名安装器。
程序本身无需 Go、Docker、Node.js 或数据库。

在另一台电脑安装
1. 将整个 StarPort-Windows-x64-Setup.zip 复制到另一台电脑。
2. 右键 ZIP，选择“全部解压缩”。不要在压缩文件预览窗口内直接运行。
3. 打开解压后的文件夹，双击 INSTALL.cmd，无需“以管理员身份运行”。
4. 安装完成会在后台启动程序、打开管理面板，并创建两个桌面快捷方式。

安装目录
%LOCALAPPDATA%\Programs\StarPort
可把上面这一行粘贴到资源管理器地址栏，按回车打开。

日常启动 / 关闭
桌面“StarPort - Start”：启动并打开面板。
桌面“StarPort - Stop”：停止后台进程，请先结束正在进行的模型请求。
也可以在安装目录双击 start-local.cmd / stop-local.cmd。
关闭浏览器页面不会停止程序；未设置开机自启，重启电脑后需要再次手动启动。

面板与登录
默认面板：http://127.0.0.1:7863/startport/
用记事本打开安装目录中的 config.json，复制 api_key 的值登录（不含引号）。
首次启动会自动生成独立的随机管理密钥。
随后在“账号中心”或渠道设置中添加自己的平台账号 / API 凭证。
客户端使用的 agent 密钥可在面板“API 密钥”中创建。
默认 OpenAI 兼容 Base URL：http://127.0.0.1:7863/v1

数据与重装
此安装包不含打包电脑的 config.json、账号、密钥、日志或使用数据。
重复安装会保留目标电脑上的 config.json、auths 和 data。
更新不同版本前请先停止旧程序。已有程序占用 7863 端口时，新实例无法在同一地址启动。
备份时请保存安装目录中的 config.json、auths 文件夹和 data 文件夹。
安装包解压目录可在安装完成后删除；实际程序位于上述安装目录。

排错
保留安装窗口中的错误提示。运行日志在安装目录的 data\logs 内。
存活检查：http://127.0.0.1:7863/livez，正常时 alive 为 true。
首次未添加账号时 /healthz 返回 503 并不表示程序未启动。
离线安装不需要网络；调用在线模型、登录上游平台需要网络及自己的有效凭证。

项目主页：https://github.com/RisingInsight/startport
随包附有原项目 LICENSE。安装助手不会更改系统 PATH 或系统执行策略。
