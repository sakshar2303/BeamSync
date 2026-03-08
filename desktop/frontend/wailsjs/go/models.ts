export namespace main {
	
	export class ReceivedFile {
	    name: string;
	    sizeBytes: number;
	    modTime: string;
	
	    static createFrom(source: any = {}) {
	        return new ReceivedFile(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.sizeBytes = source["sizeBytes"];
	        this.modTime = source["modTime"];
	    }
	}

}

