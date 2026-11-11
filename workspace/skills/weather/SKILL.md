---
name: weather
description: 获取当前天气与预报，并已验证地点匹配（无需 API key）。
homepage: https://wttr.in/:help
metadata: {"nanobot":{"emoji":"🌤️","requires":{"bins":["curl"]}}}
---

# 天气

优先使用最可靠的地点匹配。对于中文城市名或其他非拉丁文字输入，优先使用 `wttr.in` 并传入原始查询，因为它能直接解析本地名称。只有在确认了确切城市之后，才用 Open-Meteo 获取结构化的实况天气和预报。

## 准确性规则

- 在最终回答中始终复述匹配到的地点、地区/国家以及观测时间。
- 不要盲目相信第一个地理编码结果。检查 `country`、`admin1`、`admin2` 和 `population`。
- 对于中文城市查询，除非第一条结果明显正确，否则不要把汉字直接发给 Open-Meteo 地理编码接口。优先用原始中文名调用 `wttr.in`，或改用英文/拼音城市名进行地理编码。
- 如果仍存在多个可能的匹配，追问一个问题，或明确说明所做的假设。
- 调用 Open-Meteo 时使用 `timezone=auto`，以保证报告的时间与地点一致。

## wttr.in（最适合直接用城市名查询）

快速查看实况天气：
```bash
curl -s "https://wttr.in/London?format=%l:+%c+%t+%h+%w"
```

中文城市示例：
```bash
curl -s "https://wttr.in/%E6%88%90%E9%83%BD?format=%l:+%c+%t+%h+%w"
curl -s "https://wttr.in/%E4%B8%8A%E6%B5%B7?format=%l:+%c+%t+%h+%w"
```

需要更多细节时使用 JSON 输出：
```bash
curl -s "https://wttr.in/Chengdu?format=j1"
```

提示：
- 对空格做 URL 编码：`New York` -> `New+York`
- 发送请求前先对非 ASCII 文本做 URL 编码
- 使用 `?m` 表示公制单位，`?u` 表示美制单位

## Open-Meteo（最适合结构化预报）

1. 对城市进行地理编码，并核对返回的地点元数据：
```bash
curl -s "https://geocoding-api.open-meteo.com/v1/search?name=Chengdu&count=3&language=en&format=json"
```

2. 用核实过的坐标查询实况天气和今日预报：
```bash
curl -s "https://api.open-meteo.com/v1/forecast?latitude=30.66667&longitude=104.06667&current=temperature_2m,relative_humidity_2m,weather_code,wind_speed_10m&daily=weather_code,temperature_2m_max,temperature_2m_min&forecast_days=1&timezone=auto"
```

重要：
- 对于 `成都` 这类中文输入，地理编码 `name=%E6%88%90%E9%83%BD` 可能先返回规模较小的同名地点。在确认 `Chengdu` 对应中国四川后，优先使用 `Chengdu`。
- 如果地理编码结果可疑，改用 `wttr.in` 按原始城市名查询，而不是给出一个很可能错误的结果。

文档：https://open-meteo.com/en/docs