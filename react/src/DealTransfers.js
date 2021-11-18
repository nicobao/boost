import React from "react";
import { Chart } from "react-google-charts";

export function DealTransfersPage(props) {
    return <div>
        <Chart
            width={800}
            height={'400px'}
            chartType="AreaChart"
            loader={<div>Loading Chart</div>}
            data={[
                ['Time', '70e86f0b', '114aa887', '19abf7bf', 'b26b276a'],
                ['10:15:05', 800, 400, 230, 0],
                ['10:15:06', 840, 460, 315, 0],
                ['10:15:07', 660, 600, 283, 400],
                ['10:15:08', 1030, 540, 322, 224],
                ['10:15:09', 1020, 560, 325, 300],
                ['10:15:10', 984, 425, 299, 380],
            ]}
            options={{
                hAxis: { titleTextStyle: { color: '#333' } },
                isStacked: true,
            }}
        />
    </div>
}
