export namespace main {
	
	export class LogLine {
	    seq: number;
	    time: string;
	    level: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new LogLine(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.time = source["time"];
	        this.level = source["level"];
	        this.message = source["message"];
	    }
	}
	export class PathView {
	    id: number;
	    name: string;
	    kind: string;
	    weight: number;
	    healthy: boolean;
	    srtt_ms: number;
	    jitter_ms: number;
	    loss: number;
	    rx_bps: number;
	    last_seen?: string;
	
	    static createFrom(source: any = {}) {
	        return new PathView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.kind = source["kind"];
	        this.weight = source["weight"];
	        this.healthy = source["healthy"];
	        this.srtt_ms = source["srtt_ms"];
	        this.jitter_ms = source["jitter_ms"];
	        this.loss = source["loss"];
	        this.rx_bps = source["rx_bps"];
	        this.last_seen = source["last_seen"];
	    }
	}
	export class Profile {
	    path: string;
	    server: string;
	    paths: string[];
	    reorder_delay_ms: number;
	
	    static createFrom(source: any = {}) {
	        return new Profile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.server = source["server"];
	        this.paths = source["paths"];
	        this.reorder_delay_ms = source["reorder_delay_ms"];
	    }
	}
	export class ReorderView {
	    delivered: number;
	    duplicates: number;
	    late: number;
	    skipped: number;
	    pending: number;
	
	    static createFrom(source: any = {}) {
	        return new ReorderView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.delivered = source["delivered"];
	        this.duplicates = source["duplicates"];
	        this.late = source["late"];
	        this.skipped = source["skipped"];
	        this.pending = source["pending"];
	    }
	}
	export class StatusView {
	    state: string;
	    version: string;
	    profile: Profile;
	    last_error?: string;
	    server?: string;
	    tunnel_ip?: string;
	    prefix_len?: number;
	    mtu?: number;
	    connected_for?: string;
	    reconnecting?: boolean;
	    retry_attempt?: number;
	    paths: PathView[];
	    reorder: ReorderView;
	    data_seq: number;
	    control_seq: number;
	
	    static createFrom(source: any = {}) {
	        return new StatusView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.version = source["version"];
	        this.profile = this.convertValues(source["profile"], Profile);
	        this.last_error = source["last_error"];
	        this.server = source["server"];
	        this.tunnel_ip = source["tunnel_ip"];
	        this.prefix_len = source["prefix_len"];
	        this.mtu = source["mtu"];
	        this.connected_for = source["connected_for"];
	        this.reconnecting = source["reconnecting"];
	        this.retry_attempt = source["retry_attempt"];
	        this.paths = this.convertValues(source["paths"], PathView);
	        this.reorder = this.convertValues(source["reorder"], ReorderView);
	        this.data_seq = source["data_seq"];
	        this.control_seq = source["control_seq"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

