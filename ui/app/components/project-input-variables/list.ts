import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { Ref, Project, Variable } from 'waypoint-pb';

interface ProjectSettingsArgs {
  project;
}


export default class ProjectInputVariablesListComponent extends Component<ProjectSettingsArgs> {
  @tracked args;
  @tracked project;
  @tracked variablesList: Array<Variable.AsObject>;
  @tracked isCreating: boolean;
  @tracked activeVariable;

  constructor(owner: any, args: any) {
    super(owner, args);
    let { project } = this.args;
    this.project = project;
    this.variablesList = this.project.variablesList;
    this.activeVariable = null;
    this.isCreating = false;
  }

  @action
  addVariable() {
    this.isCreating = true;
    let newVar = new Variable();
    newVar.setName('Foo');
    newVar.setStr('keyvalue')
    this.activeVariable = newVar.toObject();
    this.variablesList = [newVar.toObject(), ...this.variablesList];
  }

  @action
  cancelCreate() {
    this.activeVariable = null;
    this.isCreating = false;
  }

  @action
  async saveVariableSettings(e: Event) {
    e.preventDefault();
  }
}
