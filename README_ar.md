<div align="center">

<img src="imgs/CrossLinkBanner.png" alt="CrossLink Banner">

<br/>

<img src="https://img.shields.io/badge/Go-1.22+-00ADD8?style=for-the-badge&logo=go" alt="Go Version">
<img src="https://goreportcard.com/badge/github.com/HotRiceNoodles/CrossLink?style=for-the-badge" alt="Go Report Card">
<img src="https://img.shields.io/badge/License-Apache%202.0-blue?style=for-the-badge" alt="License">
<img src="https://img.shields.io/badge/PRs-Welcome-brightgreen?style=for-the-badge" alt="PRs Welcome">

<br/>

<img src="https://img.shields.io/github/stars/HotRiceNoodles/CrossLink?style=for-the-badge" alt="Stars">
<img src="https://img.shields.io/github/issues/HotRiceNoodles/CrossLink?style=for-the-badge" alt="Issues">
<img src="https://img.shields.io/github/discussions/HotRiceNoodles/CrossLink?style=for-the-badge" alt="Discussions">
<img src="https://img.shields.io/github/last-commit/HotRiceNoodles/CrossLink?style=for-the-badge" alt="Last Commit">

<br/>
<br/>

# CrossLink

### بوابة واحدة. كل النماذج. صفر قيد.

**بوابة واجهة برمجة تطبيقات متوافقة مع OpenAI و Anthropic**

[English](README.md) | [中文](README_zh.md) | [العربية](#البدء-السريع)

وكيل (Proxy) موحّد بترجمة حقيقية ثنائية الاتجاه بين البروتوكولات، و failover
يصنّف الأخطاء، وبوابة MCP بتحكم وصول لكل أداة، وحواجز وقائية (guardrails) قابلة
للإضافة، ولوحة تحكم مدمجة — لـ OpenAI و Anthropic و Azure و DeepSeek و Qwen و
Ollama وأي مزوّد متوافق مع OpenAI.

[البدء السريع](#البدء-السريع) · [أبرز المزايا](#أبرز-المزايا) · [الميزات](#الميزات) · [البنية](#البنية) · [التوثيق](docs/README.md) · [المساهمة](CONTRIBUTING.md)

</div>

---

<div dir="rtl">

## لماذا CrossLink؟

لكل مزوّد نماذج لغوية (LLM) صيغة API مختلفة، وآلية مصادقة مختلفة، ومجموعة ميزات
مختلفة. تعديل كودك ليتوافق مع كل واحد منها مُملّ، عرضة للأخطاء، ويُقيّدك لدى المزوّد.

CrossLink هو **محوّل عام** بين تطبيقك وأي مزوّد نماذج لغوية:

- **نقطة نهاية واحدة** — يتحدث كودك إلى واجهة واحدة بصيغة OpenAI أو Anthropic
- **أي مزوّد** — تُوجَّه الطلبات إلى OpenAI أو Anthropic أو Azure أو DeepSeek أو Qwen أو
  Ollama أو أي خدمة متوافقة مع OpenAI
- **ترجمة ثنائية الاتجاه حقيقية** — تحويل كامل لتدفق SSE بين OpenAI و Anthropic و
  Responses API، يشمل استدعاء الأدوات (tool use) والتفكير الممتد (extended thinking)
- **مرونة قائمة على التصنيف** — Failover ليس إعادة محاولة عمياء. تُصنَّف الأخطاء إلى
  دائمة (حصة/فوترة — ضربة واحدة وفتحة طويلة) أو عابرة (حد المعدل — قائمة على عتبة)،
  فتذهب إعادة المحاولة حيث يمكنها أن تنجح فعلاً
- **توجيه قابل للملاحظة** — كل استجابة تحمل ترويسات `x-crosslink-fallback-*`، وواجهة
  إحصاءات تُظهر التوزيع الفعلي مقابل المُهيّأ لترى الانحراف

---

## أبرز المزايا

ما يميّز CrossLink — كل ميزة مدعومة بالكود لا بالتسويق.

- 🔁 **ترجمة تدفق ثنائية الاتجاه** — OpenAI ↔ Anthropic ↔ Responses API في الاتجاهات
  الثلاثة، عبر آلة حالة حقيقية (لا إعادة كتابة على مستوى الطلب). تتعامل مع كتل التفكير،
  ووسائط الأدوات بـ JSON الجزئي، وعدّ التوكنات أثناء التدفق. → [البنية](docs/architecture.md)
- 🛡️ **Failover مصنّف للأخطاء** — جدول قواعد في قاعدة البيانات يميّز الأخطاء الدائمة عن
  العابرة؛ فحص half-open أحادي (single-flight) يمنع الاندفاع عند عودة مزوّد غير مستقر.
  → [التوجيه و Failover](docs/routing-and-failover.md)
- 🔌 **بوابة MCP بتحكم وصول لكل أداة** — ليست وكيلاً رقيقاً. تجريد نقل، واكتشاف أدوات
  مع تخزين مؤقت singleflight، وبيانات اعتماد مُشفّرة، وقوائم سماح/منع لكل مفتاح أو فريق
  أو دور. → [بوابة MCP](docs/mcp-gateway.md)
- 🚧 **حواجز وقائية كسجلّ إضافات** — أضف أي محرّك (تعبيرات منتظمة، API خارجي، ML مستقبلاً)
  عبر `RegisterEngine`. الإجراءات: block / log / mask. إعداد لكل نموذج، fail-open أو
  fail-closed. → [البنية](docs/architecture.md)
- 📊 **شفافية التوجيه** — ترويسات `x-crosslink-fallback-model` / `x-crosslink-fallback-count`
  على كل استجابة، بالإضافة إلى واجهة توزيع التوجيه التي تُظهر الوزن المُهيّأ مقابل الفعلي،
  والانحراف، ومعدل الخطأ، والكمون لكل مزوّد.
- ❤️‍🩹 **عدّادات إرسال ذاتية الاستشفاء** — حدود التزامن/RPM لكل (مزوّد، نموذج) عبر Redis Lua
  مع نبضة TTL؛ عملية تحطّمة لا يمكنها ترك مزوّد عالقاً في حالة "مشغول".
- 🇨🇳 **تشفير صيني وطني (GM) وجاهز للعزل** — وضع SM2/SM3/SM4 (يشمل توقيع JWT بـ HMAC-SM3)
  و CAPTCHA منزلق مُستضاف ذاتياً. بلا اعتماد على reCAPTCHA/hCaptcha — يُنشر دون اتصال
  بالكامل لتوافق 信创 (الابتكار التكنولوجي المحلي الصيني). → [النشر](docs/deployment.md)
- 🎁 **نواة مفتوحة المصدر سخيّة** — إصدار Community (Apache 2.0) يضم 39 إجراءً يشمل MCP
  و RBAC وإحصاءات التوجيه وقواعد الأخطاء. Pro يضيف guardrails/playground/secrets؛
  Enterprise يضيف متعدد المؤسسات والتدقيق والميزانيات.

---

## الميزات

### البوابة الأساسية

- **بروتوكول مزدوج** — `/v1/chat/completions` (OpenAI) و `/v1/messages` (Anthropic) مع
  ترجمة ثنائية الاتجاه تلقائية، تشمل التدفق
- **متعدد المزوّدين** — OpenAI و Anthropic و Azure OpenAI و DeepSeek و Qwen و Moonshot و
  Ollama وأي مزوّد متوافق مع OpenAI
- **توجيه ذكي** — 6 استراتيجيات: عشوائي موزون، دوروربين، أقل كمون، أقل تكلفة، أقل ازدحام،
  ونشر الكناري
- **Failover تلقائي** — سلاسل تراجع متعددة المزوّدين مع قواطع دوائر، وسياسات إعادة محاولة
  قابلة للإعداد (تراجع أسي/ثابت/خطي)، وتصنيف أخطاء
- **تخزين مؤقت للاستجابات** — تخزين عبر Redis مع TTL لكل نموذج، وضغط gzip، وعزل مفتاح
  التخزين لكل مستخدم

### الأمان والتحكم

- **تحديد المعدل** — حدود RPM/TPM لكل مفتاح مع تحكم تزامن عام (2000)
- **RBAC** — تحكم وصول قائم على الأدوار للمزوّدين والنماذج ومفاتيح API و MCP
- **إدارة الميزانيات** — حدود ميزانية لكل مفتاح ولكل فريق مع قاطع دائرة تلقائي
- **حواجز وقائية** — إطار محرّك أمان محتوى قابل للإضافة بقواعد وإجراءات قابلة للإعداد
- **مرونة تشفير** — معيارية (SHA-256/RSA/AES) أو تشفير صيني وطني (SM3/SM2/SM4)

### القابلية للملاحظة

- **تحليلات الاستخدام** — استخدام التوكنات، وتتبّع التكلفة، ومقاييس الكمون، ومعدّلات
  أصابة التخزين المؤقت، وعدّادات التراجع/إعادة المحاولة لكل طلب
- **مقاييس Prometheus** — نقطة نهاية مقاييس مدمجة للمراقبة
- **OpenTelemetry** — دعم تتبّع موزّع
- **تسجيل منظّم** — تسجيل JSON بسياق الطلب

### بوابة MCP

- **Model Context Protocol** — نقل HTTP/SSE، واكتشاف أدوات مع تخزين مؤقت، وفحوص صحة
- **إدارة الصلاحيات** — تحكم وصول للأدوات لكل مبدأ (سماح/منع لكل مفتاح أو فريق أو دور)
- **تسجيل الاستدعاءات** — تسجيل شامل لاستدعاءات الأدوات مع تقسيم شهري وتنظيف تلقائي

### العمليات

- **لوحة تحكم Vue 3** — واجهة ويب مدمجة لإدارة المزوّدين والنماذج والمفاتيح والاستخدام و
  MCP ([CrossLink-UI-Standard](https://github.com/HotRiceNoodles/CrossLink-UI-Standard))
- **متعدد النسخ** — Redis Pub/Sub لمزامنة سجلّ المزوّدين ودوروربين موزّع
- **إيقاف سلس** — تصريف من 5 مراحل: تدفقات SSE الجارية → إيقاف HTTP → تصريف العمال →
  إلغاء goroutines الخلفية → تنظيف قاعدة البيانات
- **نشر بأمر واحد** — Docker Compose يُقوم البوابة والواجهة الأمامية و PostgreSQL و Redis
  بأمر واحد

---

## البنية

<p align="center">
  <img src="imgs/Architecture.png" alt="بنية CrossLink" width="720">
</p>

طالع [البنية](docs/architecture.md) لتدفّق الطلبات، وآلة حالة المترجم، وميزانية مهلة
محرك التراجع، والإيقاف السلس من 5 مراحل.

---

## معاينة لوحة التحكم

<p align="center">
  <img src="imgs/Dashboard.png" alt="نظرة عامة على لوحة التحكم" width="720">
</p>
<p align="center"><em>لوحة التحكم: حجم الطلبات والتكلفة واستخدام التوكنات والكمون ومعدل الخطأ وتوزيع النماذج بنظرة واحدة.</em></p>

<p align="center">
  <img src="imgs/MCP.png" alt="إدارة خوادم MCP" width="720">
</p>
<p align="center"><em>إدارة خوادم MCP: السجلّ بأنواع النقل (HTTP/SSE/stdio) وحالة الصحة وإعداد لكل خادم.</em></p>

<p align="center">
  <img src="imgs/Provider.png" alt="إعداد المزوّدين والنماذج" width="720">
</p>
<p align="center"><em>إعداد المزوّدين والنماذج: الوزن والأولوية والتسعير واستراتيجية التوجيه لكل نموذج.</em></p>

---

## البدء السريع

### المتطلبات المسبقة

- Go 1.22+ (للبناء من المصدر)
- PostgreSQL 14+
- Redis 7+

### Docker Compose (موصى به)

تُبنى الواجهة الأمامية ([CrossLink-UI-Standard](https://github.com/HotRiceNoodles/CrossLink-UI-Standard))
والخلفية معاً. أمر واحد يُشغّل كل شيء:

```bash
git clone https://github.com/HotRiceNoodles/CrossLink.git
cd CrossLink
docker compose -f deployments/docker-compose.dev.yaml up --build
```

لوحة التحكم الأمامية وبوابة الـ API متاحة على `http://localhost` (المنفذ 80).

> **شبكة الصين؟** استخدم `docker compose -f deployments/docker-compose.cn.yaml up --build` مع مرايا Go و npm مُهيّأة مسبقاً.

### البناء من المصدر

```bash
git clone https://github.com/HotRiceNoodles/CrossLink.git
cd CrossLink
cp configs/config.example.yaml configs/config.yaml
# عدّل config.yaml بإعدادات قاعدة البيانات و Redis
make build
./bin/crosslink
```

### أرسل أول طلب

أنشئ مفتاح API عبر لوحة التحكم (`http://localhost:8080`)، ثم جرّبه خلال 30 ثانية:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer cl-your-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

**OpenAI SDK (Python)**

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="cl-your-api-key"
)

response = client.chat.completions.create(
    model="deepseek-chat",
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)
```

**Anthropic SDK (Python)**

```python
import anthropic

client = anthropic.Anthropic(
    base_url="http://localhost:8080",
    api_key="cl-your-api-key"
)

message = client.messages.create(
    model="claude-sonnet-4-20250514",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello!"}]
)
print(message.content[0].text)
```

---

## الإعدادات

جميع الإعدادات في `configs/config.yaml`. كل قيمة يمكن تجاوزها بمتغيرات البيئة باستخدام
البادئة `CL_` (مثل `CL_DATABASE_HOST` و `CL_REDIS_PORT`).

```yaml
server:
  port: 8080
  read_timeout: 30s
  write_timeout: 120s

database:
  host: localhost
  port: 5432
  user: crosslink
  password: crosslink
  dbname: crosslink
  sslmode: disable

redis:
  host: localhost
  port: 6379

gateway:
  auth_key: "cl-change-me"

admin:
  username: admin
  password: changeme
  jwt_secret: "change-me-to-a-random-secret"

cache:
  enabled: true
  default_ttl: 5m

mcp:
  enabled: true
  health_check_interval: 30s

crypto:
  mode: standard    # standard (SHA-256/RSA/AES) أو gm (SM3/SM2/SM4)
```

### تهيئة المزوّدين

تُحمّل المزوّدون من `configs/providers.yaml` عند أول تشغيل:

```yaml
providers:
  - name: deepseek
    adapter_type: openai_compatible
    base_url: https://api.deepseek.com/v1
    api_key: ${DEEPSEEK_API_KEY}
    models:
      - name: deepseek-chat
        provider_model: deepseek-chat
```

---

## نقاط النهاية (API)

### البوابة (تتطلب مفتاح API)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/chat/completions` | محادثة متوافقة مع OpenAI (تدفق وغير تدفق) |
| `POST` | `/v1/messages` | رسائل متوافقة مع Anthropic (تدفق وغير تدفق) |
| `GET` | `/v1/models` | سرد النماذج المتاحة |

### بوابة MCP

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/mcp/:server` | تمرير JSON-RPC لـ MCP |
| `GET` | `/mcp/:server` | نقل SSE لـ MCP |

### الإدارة (تتطلب JWT)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/admin/api/auth/login` | تسجيل الدخول |
| `CRUD` | `/admin/api/providers` | إدارة المزوّدين (اختبار عبر `POST /:id/test`) |
| `CRUD` | `/admin/api/models` | إدارة تخطيط النماذج |
| `CRUD` | `/admin/api/keys` | إدارة مفاتيح API (إعادة توليد عبر `POST /:id/regenerate`) |
| `GET` | `/admin/api/usage` | سجلات الاستخدام بتصفية متعددة الأبعاد |
| `GET` | `/admin/api/routing/stats` | توزيع التوجيه: المُهيّأ مقابل الفعلي لكل مزوّد |
| `CRUD` | `/admin/api/mcp/servers` | إدارة خوادم MCP |
| `GET` | `/admin/api/mcp/servers/:id/tools` | سرد الأدوات على خادم MCP |

مرجع الـ API الكامل — بما يشمل أشكال الطلب/الاستجابة وأكواد الأخطاء وترويسات
`x-crosslink-fallback-*` — في [docs/api-reference.md](docs/api-reference.md).

---

## النشر

### Docker Compose للإنتاج

```bash
docker compose -f deployments/docker-compose.prod.yaml up -d --build
```

### شبكة الصين

استخدم متغيّر CN مع وكيل Go (`goproxy.cn`) ومرآة npm (`registry.npmmirror.com`):

```bash
docker compose -f deployments/docker-compose.cn.yaml up --build
```

### Nginx · Caddy · Systemd · GM

إعداد Nginx جاهز للإنتاج (TLS، ترويسات أمان، تدفق SSE) في `deployments/nginx/`، و
Caddyfile لـ HTTPS تلقائي، ووحدة systemd، ونشر GM مخصّص (SM2/SM3/SM4) مع GmSSL/Nginx
+ TLCP في `deployments/gm/`.

→ طالع [النشر](docs/deployment.md) لكل الخيارات وملاحظات التوسّع متعدد النسخ.

---

## التوثيق

- [البنية](docs/architecture.md) — تدفّق الطلبات، آلة حالة المترجم، محرك التراجع
- [التوجيه و Failover](docs/routing-and-failover.md) — الاستراتيجيات، قاطع الدائرة، تصنيف الأخطاء
- [بوابة MCP](docs/mcp-gateway.md) — النقل، تحكم وصول لكل أداة، بيانات اعتماد مُشفّرة
- [مرجع الـ API](docs/api-reference.md) — مرجع كامل لنقاط النهاية
- [النشر](docs/deployment.md) — كل متغيّرات النشر

---

## خارطة الطريق

CrossLink قيد التطوير النشط. التركيز الحالي:

- [x] حواجز وقائية للمزوّدين مع توجيه واعٍ بالصحة
- [x] ملاحظة توزيع التوجيه (`/admin/api/routing/stats`)
- [x] عدّادات تزامن ذاتية الاستشفاء (نبضة TTL)
- [x] ترجمة OpenAI Responses API (تدفق + غير تدفق)
- [x] CAPTCHA منزلق مُستضاف ذاتياً (جاهز للعزل)
- [ ] قواعد تنبيه حواجز المزوّدين (Enterprise) — قيد التنفيذ
- [ ] توسيع منظومة محرّكات الحواجز (مصنّفات ML)
- [ ] ميزانيات وتدقيق متعدد الفرق (Enterprise)

لديك طلب؟ افتح [نقاشاً](https://github.com/HotRiceNoodles/CrossLink/discussions) أو
[طلب ميزة](https://github.com/HotRiceNoodles/CrossLink/issues/new/choose).

---

## المجتمع والدعم

- 💬 **الأسئلة والأفكار** — [GitHub Discussions](https://github.com/HotRiceNoodles/CrossLink/discussions)
- 🐛 **تقارير الأخطاء** — [افتح issue](https://github.com/HotRiceNoodles/CrossLink/issues/new/choose) (استخدم قالب bug-report)
- 🔒 **بلاغات الأمان** — طالع [SECURITY.md](SECURITY.md) للإفصاح الخاص
- ⭐ **يعجبك المشروع؟** امنحه نجمة — يساعد الآخرين على اكتشاف CrossLink.

---

## المساهمة

نرحّب بمساهمات بكل الأحجام — إصلاحات أخطاء، ميزات، توثيق، أو أفكار.

1. اعمل Fork للمستودع
2. أنشئ فرع ميزة (`git checkout -b feature/my-feature`)
3. التزم تغييراتك (`git commit -m 'Add some feature'`)
4. ادفع للفرع (`git push origin feature/my-feature`)
5. افتح Pull Request

طالع [CONTRIBUTING.md](CONTRIBUTING.md) للإرشادات المفصّلة.

### التطوير

```bash
make build          # بناء الملف التنفيذي (bin/crosslink)
make run            # تشغيل الخادم
make test           # تشغيل كل الاختبارات
make lint           # تشغيل golangci-lint
make clean          # إزالة نتاجات البناء
```

---

## الأمان

طالع [SECURITY.md](SECURITY.md) لسياسة الأمان وتعليمات الإبلاغ عن الثغرات.

---

## الترخيص

[Apache License 2.0](LICENSE)

</div>
