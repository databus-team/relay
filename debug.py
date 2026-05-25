from urllib.parse import unquote

from fastmcp import Client

cookies = {
    "code-server-session": "%24argon2id%24v%3D19%24m%3D65536%2Ct%3D3%2Cp%3D4%24975rIwxuQLR07cGJvewg9Q%24Rw3FI2l3AcoFxeTf7vvf6c1ufbKMR6XFEoWQAYlDB7w",
    # '_dvp_session_': 'ahKAWWNoZW50aWFudGlhbkBteWhleGluLmNvbSqqANt6YyUpkHd1UOEcvJNXyl90NMpnGmai1ixMBCSZ',
    # '_dvp_email_': 'chentiantian@myhexin.com',
    # '_ddv_email_': 'chentiantian@myhexin.com',
    # '_ddv_session_': 'ahKAeWNoZW50aWFudGlhbkBteWhleGluLmNvbbd1iKDE9ii29Q2SGfsIwpCaTSQOi0htc9sWfoaBjnxa',
    # 'userid': 'chentiantian@myhexin.com',
    # '_dvpa_session_': 'ahKAfGNoZW50aWFudGlhbkBteWhleGluLmNvbWV2W91qR5Y4C0ZPuAO1xq9zpE26IUWS76obv5QT2W07',
    # '_dvpa_email_': 'chentiantian@myhexin.com',
    # '_dvp_artifact_session_': 'ahKAfGNoZW50aWFudGlhbkBteWhleGluLmNvbURoGhcoe6SEJh3l01owPY8AM1Qo_USHCSIoKFOiFY-Z',
    # '_dvp_artifact_email_': 'chentiantian@myhexin.com',
    # 'basic': 'MTc3OTUxMjkxMHxOd3dBTkRKVFYxZFpWMWRZVTB4WVNEYzFOMHhVUmxSR1UwRkNTVXRaTWt0RVEwMUJRMWRIUlRkT05GZ3lRVmxUVEU5VVJGbExNMEU9fFnDr9ysEbXWQEIYpdtc0dJgf4_bnzi2zd730XkA4eN3',
    # 'SESSIONID': 'session_1779512912269_uucr1hwk6',
}

headers = {
    "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7",
    "Accept-Language": "zh-CN,zh;q=0.9",
    "Cache-Control": "max-age=0",
    "Connection": "keep-alive",
    "Sec-Fetch-Dest": "document",
    "Sec-Fetch-Mode": "navigate",
    "Sec-Fetch-Site": "none",
    "Sec-Fetch-User": "?1",
    "Upgrade-Insecure-Requests": "1",
    "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
    "sec-ch-ua": '"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"',
    "sec-ch-ua-mobile": "?0",
    "sec-ch-ua-platform": '"macOS"',
    # 'Cookie': 'code-server-session=%24argon2id%24v%3D19%24m%3D65536%2Ct%3D3%2Cp%3D4%24975rIwxuQLR07cGJvewg9Q%24Rw3FI2l3AcoFxeTf7vvf6c1ufbKMR6XFEoWQAYlDB7w; _dvp_session_=ahKAWWNoZW50aWFudGlhbkBteWhleGluLmNvbSqqANt6YyUpkHd1UOEcvJNXyl90NMpnGmai1ixMBCSZ; _dvp_email_=chentiantian@myhexin.com; _ddv_email_=chentiantian@myhexin.com; _ddv_session_=ahKAeWNoZW50aWFudGlhbkBteWhleGluLmNvbbd1iKDE9ii29Q2SGfsIwpCaTSQOi0htc9sWfoaBjnxa; userid=chentiantian@myhexin.com; _dvpa_session_=ahKAfGNoZW50aWFudGlhbkBteWhleGluLmNvbWV2W91qR5Y4C0ZPuAO1xq9zpE26IUWS76obv5QT2W07; _dvpa_email_=chentiantian@myhexin.com; _dvp_artifact_session_=ahKAfGNoZW50aWFudGlhbkBteWhleGluLmNvbURoGhcoe6SEJh3l01owPY8AM1Qo_USHCSIoKFOiFY-Z; _dvp_artifact_email_=chentiantian@myhexin.com; basic=MTc3OTUxMjkxMHxOd3dBTkRKVFYxZFpWMWRZVTB4WVNEYzFOMHhVUmxSR1UwRkNTVXRaTWt0RVEwMUJRMWRIUlRkT05GZ3lRVmxUVEU5VVJGbExNMEU9fFnDr9ysEbXWQEIYpdtc0dJgf4_bnzi2zd730XkA4eN3; SESSIONID=session_1779512912269_uucr1hwk6',
}

# response = requests.get(
#     "https://paas.myhexin.com/devops-dev/proxy/ths-5503769556/8080/proxy/3000/",
#     cookies=cookies,
#     headers=headers,
# )


token = unquote(
    "%24argon2id%24v%3D19%24m%3D65536%2Ct%3D3%2Cp%3D4%24975rIwxuQLR07cGJvewg9Q%24Rw3FI2l3AcoFxeTf7vvf6c1ufbKMR6XFEoWQAYlDB7w"
)

config = {
    "mcpServers": {
        "weather": {
            "url": "https://paas.myhexin.com/devops-dev/proxy/ths-5503769556/8080/proxy/3000/mcp",
            "transport": "streamable-http",
            "headers": {
                "Cookie": "code-server-session=%24argon2id%24v%3D19%24m%3D65536%2Ct%3D3%2Cp%3D4%24975rIwxuQLR07cGJvewg9Q%24Rw3FI2l3AcoFxeTf7vvf6c1ufbKMR6XFEoWQAYlDB7w; _dvp_session_=ahKAWWNoZW50aWFudGlhbkBteWhleGluLmNvbSqqANt6YyUpkHd1UOEcvJNXyl90NMpnGmai1ixMBCSZ; _dvp_email_=chentiantian@myhexin.com; _ddv_email_=chentiantian@myhexin.com; _ddv_session_=ahKAeWNoZW50aWFudGlhbkBteWhleGluLmNvbbd1iKDE9ii29Q2SGfsIwpCaTSQOi0htc9sWfoaBjnxa; userid=chentiantian@myhexin.com; _dvpa_session_=ahKAfGNoZW50aWFudGlhbkBteWhleGluLmNvbWV2W91qR5Y4C0ZPuAO1xq9zpE26IUWS76obv5QT2W07; _dvpa_email_=chentiantian@myhexin.com; _dvp_artifact_session_=ahKAfGNoZW50aWFudGlhbkBteWhleGluLmNvbURoGhcoe6SEJh3l01owPY8AM1Qo_USHCSIoKFOiFY-Z; _dvp_artifact_email_=chentiantian@myhexin.com; basic=MTc3OTUxMjkxMHxOd3dBTkRKVFYxZFpWMWRZVTB4WVNEYzFOMHhVUmxSR1UwRkNTVXRaTWt0RVEwMUJRMWRIUlRkT05GZ3lRVmxUVEU5VVJGbExNMEU9fFnDr9ysEbXWQEIYpdtc0dJgf4_bnzi2zd730XkA4eN3; SESSIONID=session_1779512912269_uucr1hwk6",
            },
        },
    }
}


async def main():
    async with Client(config) as client:
        print(client.is_connected())


if __name__ == "__main__":
    import asyncio

    asyncio.run(main())
