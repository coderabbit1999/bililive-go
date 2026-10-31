import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Alert, Button, Card, Col, Empty, Row, Select, Space, Statistic, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ReloadOutlined, RiseOutlined } from '@ant-design/icons';
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';

interface Sample {
    ts: string;
    ts_unix: number;
    offset_s?: number;
    room_id: string;
    host: string;
    title: string;
    live_status: number;
    popularity: number;
    watched: number;
    likes: number;
    audience?: number;
    recording: boolean;
}

interface MonitorStatus {
    running: boolean;
    output: string;
    interval_sec: number;
    last_sample_at: string;
    last_error: string;
    room_count: number;
    sample_count: number;
}

const MonitorPage: React.FC = () => {
    const [samples, setSamples] = useState<Sample[]>([]);
    const [status, setStatus] = useState<MonitorStatus | null>(null);
    const [roomID, setRoomID] = useState<string>('');
    const [loading, setLoading] = useState(false);

    const load = useCallback(async () => {
        setLoading(true);
        try {
            const res = await fetch('/api/monitor/samples?limit=1000');
            const data = await res.json();
            setSamples(data.samples || []);
            setStatus(data.status || null);
        } catch {
            // 网络短暂不可用时保留上一次数据
        } finally {
            setLoading(false);
        }
    }, []);

    useEffect(() => {
        load();
        const timer = setInterval(load, 10000);
        return () => clearInterval(timer);
    }, [load]);

    const rooms = useMemo(() => {
        const map = new Map<string, string>();
        samples.forEach((s) => map.set(s.room_id, s.host || s.room_id));
        return Array.from(map.entries());
    }, [samples]);

    const filtered = useMemo(
        () => (roomID ? samples.filter((s) => s.room_id === roomID) : samples),
        [samples, roomID]
    );

    const latest = filtered.length > 0 ? filtered[filtered.length - 1] : null;

    const chartData = filtered.slice(-300).map((s) => ({
        time: s.ts.length >= 19 ? s.ts.slice(11) : s.ts,
        人气: s.popularity,
        房间观众: s.audience ?? null,
    }));

    const columns: ColumnsType<Sample> = [
        { title: '时间', dataIndex: 'ts', key: 'ts', width: 170 },
        { title: '主播', dataIndex: 'host', key: 'host', width: 140 },
        { title: '标题', dataIndex: 'title', key: 'title', ellipsis: true },
        {
            title: '人气',
            dataIndex: 'popularity',
            key: 'popularity',
            width: 110,
            sorter: (a, b) => a.popularity - b.popularity,
            render: (v: number) => v.toLocaleString(),
        },
        {
            title: '房间观众',
            dataIndex: 'audience',
            key: 'audience',
            width: 100,
            render: (v?: number) => (v === undefined || v === null ? '-' : v.toLocaleString()),
        },
        {
            title: '看过',
            dataIndex: 'watched',
            key: 'watched',
            width: 110,
            render: (v: number) => v.toLocaleString(),
        },
        {
            title: '点赞',
            dataIndex: 'likes',
            key: 'likes',
            width: 110,
            render: (v: number) => v.toLocaleString(),
        },
        {
            title: '距开录',
            dataIndex: 'offset_s',
            key: 'offset_s',
            width: 110,
            render: (v?: number) => (v === undefined || v === null ? '-' : `${Math.floor(v / 60)}分${v % 60}秒`),
        },
        {
            title: '录制中',
            dataIndex: 'recording',
            key: 'recording',
            width: 90,
            render: (v: boolean) => (v ? <Tag color="green">是</Tag> : <Tag>否</Tag>),
        },
    ];

    return (
        <div style={{ padding: 16 }}>
            <Card
                title={
                    <Space>
                        <RiseOutlined />
                        人气监控
                    </Space>
                }
                extra={
                    <Button icon={<ReloadOutlined />} onClick={load} loading={loading}>
                        刷新
                    </Button>
                }
            >
                {status && !status.running && <Alert type="warning" showIcon message="人气监控未运行" style={{ marginBottom: 16 }} />}
                {status?.last_error && (
                    <Alert type="error" showIcon message={`最近一次采样失败：${status.last_error}`} style={{ marginBottom: 16 }} />
                )}
                <Row gutter={16} style={{ marginBottom: 16 }}>
                    <Col span={6}>
                        <Statistic title="监控房间" value={status?.room_count ?? 0} />
                    </Col>
                    <Col span={6}>
                        <Statistic title="最新人气" value={latest?.popularity ?? 0} />
                    </Col>
                    <Col span={6}>
                        <Statistic title="最新观众" value={latest?.audience ?? 0} />
                    </Col>
                    <Col span={6}>
                        <Statistic title="最近采样" value={status?.last_sample_at || '-'} />
                    </Col>
                </Row>
                <Space style={{ marginBottom: 16 }} wrap>
                    <span>房间：</span>
                    <Select
                        style={{ minWidth: 220 }}
                        value={roomID}
                        onChange={setRoomID}
                        options={[{ value: '', label: '全部房间' }, ...rooms.map(([id, name]) => ({ value: id, label: `${name} (${id})` }))]}
                    />
                    <Typography.Text type="secondary">数据文件：{status?.output || 'data/viewer_log.csv'}（人气每 {status?.interval_sec ?? 10} 秒、房间观众每 15 秒更新）</Typography.Text>
                </Space>

                {chartData.length === 0 ? (
                    <Empty description="还没有采样数据，等下一次采样或先添加直播间" style={{ padding: 40 }} />
                ) : (
                    <div style={{ width: '100%', height: 280 }}>
                        <ResponsiveContainer>
                            <LineChart data={chartData} margin={{ top: 8, right: 24, left: 8, bottom: 8 }}>
                                <CartesianGrid strokeDasharray="3 3" />
                                <XAxis dataKey="time" minTickGap={40} />
                                <YAxis yAxisId="left" width={90} tickFormatter={(v: number) => (v >= 10000 ? `${Math.round(v / 10000)}万` : `${v}`)} />
                                <YAxis yAxisId="right" orientation="right" width={60} />
                                <Tooltip formatter={(v) => Number(v).toLocaleString()} />
                                <Line yAxisId="left" type="monotone" dataKey="人气" stroke="#1677ff" dot={false} strokeWidth={2} isAnimationActive={false} />
                                <Line yAxisId="right" type="monotone" dataKey="房间观众" stroke="#52c41a" dot={false} strokeWidth={2} isAnimationActive={false} connectNulls />
                            </LineChart>
                        </ResponsiveContainer>
                    </div>
                )}
            </Card>

            <Card title="最近采样" style={{ marginTop: 16 }}>
                <Table
                    rowKey={(r) => `${r.room_id}-${r.ts_unix}`}
                    size="small"
                    columns={columns}
                    dataSource={filtered.slice(-200).reverse()}
                    pagination={{ pageSize: 20, showSizeChanger: false }}
                    scroll={{ x: 900 }}
                />
            </Card>
        </div>
    );
};

export default MonitorPage;
